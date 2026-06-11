package sshclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
		{"bare host + port via field, ws", "example.com", 18080, "tcp", "myhost", "ws://example.com:18080/matrix/h/myhost/ssh"},
		{"bare host, no port, wss", "ai.dhnt.io", 0, "wss", "myhost", "wss://ai.dhnt.io/matrix/h/myhost/ssh"},
		{"https:// URL forces wss", "https://ai.dhnt.io", 0, "tcp", "myhost", "wss://ai.dhnt.io/matrix/h/myhost/ssh"},
		{"hostport in server, field-port ignored", "example.com:9090", 18080, "tcp", "myhost", "ws://example.com:9090/matrix/h/myhost/ssh"},
		{"path-encoded host", "example.com", 0, "ws", "host with space", "ws://example.com/matrix/h/host%20with%20space/ssh"},
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

// TestDialWSGatewayStatusIsHostOffline pins the gateway-status
// classification: a 502/503/504 from cloudbox (origin 502 gets
// rewritten to 504 by DO/Cloudflare in prod) means the matrix tunnel
// to the host is down — DialWS must surface EHostOfflineError instead
// of a raw handshake failure, and must NOT invoke OnElevate (a fresh
// cookie can't bring the host back).
func TestDialWSGatewayStatusIsHostOffline(t *testing.T) {
	for _, status := range []int{502, 503, 504} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			elevated := false
			_, err := DialWS(context.Background(), DialOptions{
				WSURL:  "ws" + strings.TrimPrefix(srv.URL, "http") + "/matrix/h/h1/ssh",
				Bearer: "tok",
				Host:   "h1",
				OnElevate: func(context.Context, string) (string, error) {
					elevated = true
					return "fresh", nil
				},
			})
			var off EHostOfflineError
			if !errors.As(err, &off) {
				t.Fatalf("want EHostOfflineError, got %v", err)
			}
			if off.Host != "h1" || off.Status != status {
				t.Errorf("got %+v, want Host=h1 Status=%d", off, status)
			}
			if elevated {
				t.Error("OnElevate must not run for a gateway status")
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

// TestDirectTCPIPHop wires up TWO in-test SSH servers and confirms
// that opening a direct-tcpip channel through server-A to server-B's
// TCP listener, then layering another sshclient.Client on top, lets
// us run an Exec on B via A — i.e. the ProxyJump (hop) pattern works
// end-to-end.
//
// This is the core integration test for Wave 2's hop feature.
func TestDirectTCPIPHop(t *testing.T) {
	// Server B: the final destination. Listens on its own TCP port.
	bKey := makeTestHostKey(t)
	bListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen B: %v", err)
	}
	t.Cleanup(func() { _ = bListener.Close() })
	bPort := bListener.Addr().(*net.TCPAddr).Port

	bCfg := &ssh.ServerConfig{NoClientAuth: true}
	bCfg.AddHostKey(bKey)
	go func() {
		for {
			conn, err := bListener.Accept()
			if err != nil {
				return
			}
			go runTestSSHServer(conn, bCfg, func(req *ssh.Request) (stdout, stderr []byte, exitCode uint32) {
				if string(req.Payload[4:]) == "uname" {
					return []byte("ServerB\n"), nil, 0
				}
				return nil, []byte("unknown"), 1
			})
		}
	}()

	// Server A: the jump host. Standard NoClientAuth server + handles
	// direct-tcpip channels by dialing the requested host:port (which
	// will be 127.0.0.1:bPort).
	aKey := makeTestHostKey(t)
	aListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen A: %v", err)
	}
	t.Cleanup(func() { _ = aListener.Close() })

	aCfg := &ssh.ServerConfig{NoClientAuth: true}
	aCfg.AddHostKey(aKey)
	go func() {
		conn, err := aListener.Accept()
		if err != nil {
			return
		}
		runTestSSHHopServer(conn, aCfg)
	}()

	// Client → A: standard handshake.
	clientConn, err := net.Dial("tcp", aListener.Addr().String())
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	tmp := t.TempDir()
	cbA, err := KnownHostsCallbackTOFU(filepath.Join(tmp, "kha"), "outpost-A")
	if err != nil {
		t.Fatalf("known_hosts A: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	aClient, err := Dial(ctx, Config{
		Transport: clientConn, HostAlias: "outpost-A", User: "u", HostKeyCallback: cbA,
	})
	if err != nil {
		t.Fatalf("Dial A: %v", err)
	}
	defer aClient.Close()

	// Hop: A direct-tcpip → 127.0.0.1:bPort. Layer SSH on top.
	bConn, err := aClient.DirectTCPIP(ctx, "127.0.0.1", bPort)
	if err != nil {
		t.Fatalf("DirectTCPIP: %v", err)
	}
	cbB, err := KnownHostsCallbackTOFU(filepath.Join(tmp, "khb"), "outpost-B")
	if err != nil {
		t.Fatalf("known_hosts B: %v", err)
	}
	bClient, err := Dial(ctx, Config{
		Transport: bConn, HostAlias: "outpost-B", User: "u", HostKeyCallback: cbB,
	})
	if err != nil {
		t.Fatalf("Dial B (via hop): %v", err)
	}
	defer bClient.Close()

	res, err := bClient.Exec(ctx, ExecOptions{Command: "uname", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Exec on B: %v", err)
	}
	if res.ExitCode != 0 || string(res.Stdout) != "ServerB\n" {
		t.Errorf("hop result wrong: exit=%d stdout=%q", res.ExitCode, string(res.Stdout))
	}
}

// runTestSSHHopServer is like runTestSSHServer but accepts
// direct-tcpip channels and bridges them to a raw TCP dial. Used by
// TestDirectTCPIPHop to model an outpost-A whose SSH server permits
// hop traffic to peer hosts.
func runTestSSHHopServer(conn net.Conn, cfg *ssh.ServerConfig) {
	sConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sConn.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "direct-tcpip" {
			_ = newCh.Reject(ssh.UnknownChannelType, "test server only accepts direct-tcpip")
			continue
		}
		// direct-tcpip extra data: 4-byte len-host + host + uint32 port
		// + 4-byte len-orig + orig + uint32 origPort. Parse just enough
		// to extract host + port.
		host, port, ok := parseDirectTCPIPExtra(newCh.ExtraData())
		if !ok {
			_ = newCh.Reject(ssh.ConnectionFailed, "bad direct-tcpip payload")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go ssh.DiscardRequests(chReqs)
		go func() {
			defer ch.Close()
			dst, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port)))
			if err != nil {
				return
			}
			defer dst.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(dst, ch); done <- struct{}{} }()
			go func() { _, _ = io.Copy(ch, dst); done <- struct{}{} }()
			<-done
		}()
	}
}

// parseDirectTCPIPExtra parses the RFC 4254 §7.2 direct-tcpip data:
//
//	string  host to connect
//	uint32  port to connect
//	string  originator IP
//	uint32  originator port
func parseDirectTCPIPExtra(b []byte) (host string, port uint32, ok bool) {
	if len(b) < 4 {
		return "", 0, false
	}
	hlen := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	if uint32(len(b)) < 4+hlen+4 {
		return "", 0, false
	}
	host = string(b[4 : 4+hlen])
	p := b[4+hlen : 4+hlen+4]
	port = uint32(p[0])<<24 | uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
	return host, port, true
}

// TestLocalForwardByteBridge confirms LocalForward shovels bytes
// faithfully in both directions. Uses a stub TCP echo server as the
// hop destination and a vanilla TCP client on the local side.
func TestLocalForwardByteBridge(t *testing.T) {
	// Echo destination: any byte received is echoed back.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	echoPort := echo.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()

	// SSH server-A: same hop pattern as TestDirectTCPIPHop.
	aKey := makeTestHostKey(t)
	aListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen A: %v", err)
	}
	t.Cleanup(func() { _ = aListener.Close() })
	aCfg := &ssh.ServerConfig{NoClientAuth: true}
	aCfg.AddHostKey(aKey)
	go func() {
		conn, err := aListener.Accept()
		if err != nil {
			return
		}
		runTestSSHHopServer(conn, aCfg)
	}()

	clientConn, err := net.Dial("tcp", aListener.Addr().String())
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	tmp := t.TempDir()
	cb, err := KnownHostsCallbackTOFU(filepath.Join(tmp, "kh"), "outpost-A")
	if err != nil {
		t.Fatalf("kh: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, Config{
		Transport: clientConn, HostAlias: "outpost-A", User: "u", HostKeyCallback: cb,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("local listen: %v", err)
	}
	localAddr := localListener.Addr().String()

	fwdCtx, fwdCancel := context.WithCancel(ctx)
	defer fwdCancel()
	fwdDone := make(chan error, 1)
	go func() { fwdDone <- client.LocalForward(fwdCtx, localListener, "127.0.0.1", echoPort) }()

	// Connect to the local listener, write, expect echo back.
	c, err := net.Dial("tcp", localAddr)
	if err != nil {
		t.Fatalf("dial local fwd: %v", err)
	}
	defer c.Close()
	msg := []byte("hello-tunnel\n")
	if _, err := c.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(buf, msg) {
		t.Errorf("echo mismatch: got %q want %q", buf, msg)
	}

	fwdCancel()
	select {
	case <-fwdDone:
	case <-time.After(2 * time.Second):
		t.Fatal("LocalForward did not exit within 2s of ctx cancel")
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

// TestCRLFTranslator pins the LF→CRLF rewriter that Shell() drops
// between the SSH session output and local stdout when the local
// terminal is in raw mode. The remote outpost's emulated PTY ignores
// RFC 4254 termios opcodes, so OPOST+ONLCR can't be set server-side;
// without this translator, bare \n staircases under the prompt column.
func TestCRLFTranslator(t *testing.T) {
	cases := []struct {
		name  string
		in    []string // chunks fed to Write in order
		want  string
	}{
		{
			name: "bare LF expands to CRLF",
			in:   []string{"hello\nworld\n"},
			want: "hello\r\nworld\r\n",
		},
		{
			name: "already-CRLF passes through unchanged",
			in:   []string{"hello\r\nworld\r\n"},
			want: "hello\r\nworld\r\n",
		},
		{
			name: "stray CR alone passes through (apps use it to redraw)",
			in:   []string{"100%\r"},
			want: "100%\r",
		},
		{
			name: "CR followed later by LF still translates (not part of same CRLF run)",
			in:   []string{"100%\rdone\n"},
			want: "100%\rdone\r\n",
		},
		{
			name: "chunk boundary at CR keeps the suppression for the next chunk's LF",
			in:   []string{"hello\r", "\nworld\n"},
			want: "hello\r\nworld\r\n",
		},
		{
			name: "chunk boundary mid-stream does not double-translate",
			in:   []string{"a\nb", "\nc\n"},
			want: "a\r\nb\r\nc\r\n",
		},
		{
			name: "no newlines at all is a pass-through",
			in:   []string{"plain text no eol"},
			want: "plain text no eol",
		},
		{
			name: "empty write returns nil error and writes nothing",
			in:   []string{""},
			want: "",
		},
		{
			name: "consecutive LFs each get a CR",
			in:   []string{"\n\n\n"},
			want: "\r\n\r\n\r\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tr := &crlfTranslator{w: &buf}
			for _, chunk := range tc.in {
				n, err := tr.Write([]byte(chunk))
				if err != nil {
					t.Fatalf("Write(%q): %v", chunk, err)
				}
				if n != len(chunk) {
					t.Fatalf("Write(%q): reported n=%d, want %d", chunk, n, len(chunk))
				}
			}
			if got := buf.String(); got != tc.want {
				t.Fatalf("output mismatch\n  got:  %q\n  want: %q", got, tc.want)
			}
		})
	}
}

// errWriter returns the configured error from every Write call. Used
// to confirm crlfTranslator propagates Write errors instead of
// silently swallowing them.
type errWriter struct{ err error }

func (e *errWriter) Write(p []byte) (int, error) { return 0, e.err }

func TestCRLFTranslatorPropagatesWriteError(t *testing.T) {
	wantErr := errors.New("downstream stdout closed")
	tr := &crlfTranslator{w: &errWriter{err: wantErr}}
	n, err := tr.Write([]byte("hello\nworld\n"))
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0 on error path", n)
	}
}
