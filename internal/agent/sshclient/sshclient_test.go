package sshclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// TestBuildWSURL pins the URL construction across the supported
// server-string shapes (bare hostname, host:port, full URL) and the
// protocol heuristic (wss vs ws).
func TestBuildWSURL(t *testing.T) {
	cases := []struct {
		name   string
		server string
		port   int
		proto  string
		host   string
		want   string
	}{
		{"bare host + port via field, ws", "example.com", 18080, "tcp", "myhost", "ws://example.com:18080/h/myhost/ssh"},
		{"bare host, no port, wss", "ai.dhnt.io", 0, "wss", "myhost", "wss://ai.dhnt.io/h/myhost/ssh"},
		{"https:// URL forces wss", "https://ai.dhnt.io", 0, "tcp", "myhost", "wss://ai.dhnt.io/h/myhost/ssh"},
		{"hostport in server, field-port ignored", "example.com:9090", 18080, "tcp", "myhost", "ws://example.com:9090/h/myhost/ssh"},
		{"path-encoded host", "example.com", 0, "ws", "host with space", "ws://example.com/h/host%20with%20space/ssh"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := BuildWSURL(c.server, c.port, c.proto, c.host)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestKnownHostsCallbackTOFU exercises the trust-on-first-use path:
//  1. First connect with a fresh known_hosts file pins the key.
//  2. Second connect with the same key passes.
//  3. Connect with a different key for the same alias is rejected
//     with the REMOTE HOST IDENTIFICATION HAS CHANGED message.
func TestKnownHostsCallbackTOFU(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "known_hosts")
	alias := "outpost-myhost"

	key1, key2 := makeTestHostKey(t), makeTestHostKey(t)
	remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 22}

	cb, err := KnownHostsCallbackTOFU(path, alias)
	if err != nil {
		t.Fatalf("build cb: %v", err)
	}
	// First contact: pins.
	if err := cb("informational-hostname", remote, key1.PublicKey()); err != nil {
		t.Fatalf("first-contact TOFU rejected unexpectedly: %v", err)
	}
	// Second contact same key: accepts.
	if err := cb("informational-hostname", remote, key1.PublicKey()); err != nil {
		t.Fatalf("second-contact (same key) rejected: %v", err)
	}
	// Second contact different key: hard reject.
	err = cb("informational-hostname", remote, key2.PublicKey())
	if err == nil {
		t.Fatal("expected mismatch rejection, got nil")
	}
	if !strings.Contains(err.Error(), "REMOTE HOST IDENTIFICATION HAS CHANGED") {
		t.Errorf("error message %q doesn't surface the well-known signal", err)
	}
}

// TestCappedWriterTruncates covers the per-stream byte cap used by
// Exec to bound captured output. Writes past the cap are silently
// dropped from the underlying buffer but the writer reports they
// were "written" so the SSH session doesn't backpressure (which
// would deadlock the remote command).
func TestCappedWriterTruncates(t *testing.T) {
	var buf bytes.Buffer
	cw := &cappedWriter{w: &buf, limit: 10}
	// Write 5 bytes — fits.
	n, err := cw.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("first write: n=%d err=%v", n, err)
	}
	if cw.truncated {
		t.Errorf("truncated flag set too early")
	}
	// Write 7 more bytes — 5 fit, 2 are dropped.
	n, err = cw.Write([]byte("XXXXXYY"))
	if err != nil || n != 7 {
		t.Fatalf("second write: n=%d err=%v", n, err)
	}
	if !cw.truncated {
		t.Errorf("truncated flag not set after overflow")
	}
	if buf.String() != "helloXXXXX" {
		t.Errorf("buffer got %q, want helloXXXXX", buf.String())
	}
	// Third write past cap — dropped entirely.
	n, err = cw.Write([]byte("ZZZ"))
	if err != nil || n != 3 {
		t.Fatalf("third write: n=%d err=%v", n, err)
	}
	if buf.String() != "helloXXXXX" {
		t.Errorf("buffer changed past cap: %q", buf.String())
	}
}

// TestDialAndExec wires up an in-test SSH server reachable over a
// loopback TCP listener and confirms the full path: Dial → Exec →
// exit-code propagation. Loopback (rather than net.Pipe) because
// SSH's handshake banner exchange overflows net.Pipe's zero-buffer
// synchrony and deadlocks. Loopback TCP is what production looks
// like anyway — websocket.NetConn yields a buffered net.Conn with
// the same semantics.
func TestDialAndExec(t *testing.T) {
	serverKey := makeTestHostKey(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	srvCfg := &ssh.ServerConfig{NoClientAuth: true}
	srvCfg.AddHostKey(serverKey)

	srvDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			srvDone <- err
			return
		}
		srvDone <- runTestSSHServer(conn, srvCfg, func(req *ssh.Request) (stdout, stderr []byte, exitCode uint32) {
			// SSH exec payload: 4-byte length + N bytes of command.
			if len(req.Payload) < 4 {
				return nil, []byte("bad exec payload"), 1
			}
			cmd := string(req.Payload[4:])
			switch cmd {
			case "echo hello":
				return []byte("hello\n"), nil, 0
			case "false":
				return nil, nil, 1
			default:
				return nil, []byte("unknown: " + cmd), 127
			}
		})
	}()

	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	tmp := t.TempDir()
	hostKeysPath := filepath.Join(tmp, "known_hosts")
	cb, err := KnownHostsCallbackTOFU(hostKeysPath, "outpost-mytest")
	if err != nil {
		t.Fatalf("hostkey cb: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := Dial(ctx, Config{
		Transport:        clientConn,
		HostAlias:        "outpost-mytest",
		User:             "noviadmin",
		HostKeyCallback:  cb,
		HandshakeTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	res, err := client.Exec(ctx, ExecOptions{Command: "echo hello", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode=%d, want 0", res.ExitCode)
	}
	if string(res.Stdout) != "hello\n" {
		t.Errorf("Stdout=%q, want hello\\n", string(res.Stdout))
	}

	_ = client.Close()
	select {
	case err := <-srvDone:
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			t.Logf("server returned (expected close error): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("test SSH server did not exit within 2s of client close")
	}
}

// makeTestHostKey generates a fresh ed25519 host key for one test —
// keeping the test self-contained.
func makeTestHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen ed25519: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("ssh signer: %v", err)
	}
	return signer
}

// runTestSSHServer accepts ONE inbound SSH connection on `conn`,
// services ONE session channel + ONE exec request via the provided
// handler, and returns when the connection closes. Suitable for
// unit-test scope; not production.
func runTestSSHServer(conn net.Conn, cfg *ssh.ServerConfig, handle func(req *ssh.Request) (stdout, stderr []byte, exitCode uint32)) error {
	sConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return err
	}
	defer sConn.Close()
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer ch.Close()
			for req := range chReqs {
				if req.Type != "exec" {
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
					continue
				}
				stdout, stderr, exit := handle(req)
				if len(stdout) > 0 {
					_, _ = ch.Write(stdout)
				}
				if len(stderr) > 0 {
					_, _ = ch.Stderr().Write(stderr)
				}
				if req.WantReply {
					_ = req.Reply(true, nil)
				}
				// 4-byte big-endian exit status payload, per RFC 4254 §6.10.
				payload := []byte{byte(exit >> 24), byte(exit >> 16), byte(exit >> 8), byte(exit)}
				_, _ = ch.SendRequest("exit-status", false, payload)
				return
			}
		}()
	}
	return nil
}
