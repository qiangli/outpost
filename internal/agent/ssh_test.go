package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/qiangli/outpost/internal/agent/hostauth"
)

// newTestSSHServer spins up an httptest server with the SSH handler
// mounted at /ssh and returns its ws:// URL. direct-tcpip forwarding,
// tcpip-forward (ssh -R), AND the sftp subsystem are all enabled by
// default; tests that need to assert the opt-out path use
// newTestSSHServerOpts directly.
func newTestSSHServer(t *testing.T, auth hostauth.Authenticator) (wsURL string, hostKey ssh.Signer) {
	t.Helper()
	return newTestSSHServerOpts(t, auth, false, true, true, true)
}

// newTestSSHServerOpts is the parameterized form. cloudboxStamps inserts
// an X-Periscope-Role: admin header on every request via gin middleware,
// simulating cloudbox's SSHProxy vouching for the caller.
// allowLocalForward gates `direct-tcpip` channels (stock `ssh -L` /
// `ssh -D`). allowRemoteForward gates `tcpip-forward` global requests
// (stock `ssh -R`). sftpEnabled gates whether the SSH server accepts
// the "sftp" subsystem channel; off means clients fall back to legacy
// `scp -O` over exec.
func newTestSSHServerOpts(t *testing.T, auth hostauth.Authenticator, cloudboxStamps bool, allowLocalForward bool, allowRemoteForward bool, sftpEnabled bool) (wsURL string, hostKey ssh.Signer) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if cloudboxStamps {
		engine.Use(func(c *gin.Context) {
			c.Request.Header.Set("X-Periscope-Role", "admin")
			c.Next()
		})
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	engine.GET("/ssh", sshHandler(signer, auth, "", allowLocalForward, allowRemoteForward, sftpEnabled))

	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	u.Scheme = "ws"
	u.Path = "/ssh"
	return u.String(), signer
}

// dialSSHOverWS connects to the test server, performs the SSH client
// handshake with the given username/password, and returns the ssh client.
// The caller must Close() it.
func dialSSHOverWS(t *testing.T, wsURL string, hostKey ssh.Signer, user, pass string) (*ssh.Client, error) {
	t.Helper()
	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsConn, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		return nil, err
	}
	wsConn.SetReadLimit(-1)
	netConn := websocket.NetConn(context.Background(), wsConn, websocket.MessageBinary)

	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(pass)},
		HostKeyCallback: ssh.FixedHostKey(hostKey.PublicKey()),
		Timeout:         5 * time.Second,
	}
	c, chans, reqs, err := ssh.NewClientConn(netConn, "test", cfg)
	if err != nil {
		_ = netConn.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// TestSSHHandlerShellGreets confirms the interactive-shell wiring works
// far enough that the server emits the qiangli/sh greeting banner over
// the SSH channel. We don't try to drive the shell to a clean exit
// here — the in-process parser's Interactive loop is finicky to
// terminate without a real terminal driver — that's covered by the
// manual smoke test described in the plan.
func TestSSHHandlerShellGreets(t *testing.T) {
	currentUser, err := hostauth.CurrentUser()
	if err != nil || currentUser == "" {
		t.Skip("cannot determine current OS user")
	}
	auth := hostauth.StubAuth{Want: map[string]string{currentUser: "secret"}}
	wsURL, hostKey := newTestSSHServer(t, auth)

	client, err := dialSSHOverWS(t, wsURL, hostKey, currentUser, "secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()

	if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatalf("pty: %v", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := session.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}

	// Read the first ~256 bytes within 2 s; we expect the qiangli/sh
	// greeting banner to be in there. Confirms shell.NewSession was
	// allocated, the runner started, and PTY → SSH-channel piping is
	// wired up.
	gotCh := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		n, _ := io.ReadFull(stdout, buf)
		gotCh <- string(buf[:n])
	}()
	select {
	case got := <-gotCh:
		if !strings.Contains(got, "Matrix shell") && !strings.Contains(got, currentUser) {
			t.Errorf("shell banner missing; got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shell greeting")
	}
}

// TestSSHHandlerRejectsWrongUsername — the SSH username MUST equal the
// running OS user (same gate as /auth). A right-password / wrong-username
// pair is rejected at PasswordCallback time.
func TestSSHHandlerRejectsWrongUsername(t *testing.T) {
	currentUser, err := hostauth.CurrentUser()
	if err != nil || currentUser == "" {
		t.Skip("cannot determine current OS user")
	}
	auth := hostauth.StubAuth{Want: map[string]string{currentUser: "secret"}}
	wsURL, hostKey := newTestSSHServer(t, auth)

	_, err = dialSSHOverWS(t, wsURL, hostKey, currentUser+"-impostor", "secret")
	if err == nil {
		t.Fatal("expected ssh handshake to fail with wrong username, got success")
	}
}

// TestSSHHandlerRejectsWrongPassword — Authenticator rejects → handshake
// fails after MaxAuthTries (3).
func TestSSHHandlerRejectsWrongPassword(t *testing.T) {
	currentUser, err := hostauth.CurrentUser()
	if err != nil || currentUser == "" {
		t.Skip("cannot determine current OS user")
	}
	auth := hostauth.StubAuth{Want: map[string]string{currentUser: "secret"}}
	wsURL, hostKey := newTestSSHServer(t, auth)

	_, err = dialSSHOverWS(t, wsURL, hostKey, currentUser, "wrong")
	if err == nil {
		t.Fatal("expected ssh handshake to fail with wrong password, got success")
	}
}

// TestSSHHandlerExec runs `echo hi` via the SSH exec channel (used by
// scp / rsync). Asserts stdout carries the output and exit-status is 0.
func TestSSHHandlerExec(t *testing.T) {
	currentUser, err := hostauth.CurrentUser()
	if err != nil || currentUser == "" {
		t.Skip("cannot determine current OS user")
	}
	auth := hostauth.StubAuth{Want: map[string]string{currentUser: "secret"}}
	wsURL, hostKey := newTestSSHServer(t, auth)

	client, err := dialSSHOverWS(t, wsURL, hostKey, currentUser, "secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()

	out, err := session.Output("echo hi")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(string(out), "hi") {
		t.Errorf("exec stdout = %q, want it to contain 'hi'", out)
	}
}

// TestSSHHandlerCloudboxVouchedSkipsPassword — when cloudbox stamps
// X-Periscope-Role: admin on the WSS upgrade, the SSH server flips
// NoClientAuth and the handshake completes without a password
// challenge. This is the agentic-tool path: an agent holding a cached
// matrix_elev cookie can SSH without an interactive prompt.
func TestSSHHandlerCloudboxVouchedSkipsPassword(t *testing.T) {
	currentUser, err := hostauth.CurrentUser()
	if err != nil || currentUser == "" {
		t.Skip("cannot determine current OS user")
	}
	// Auth stub that REJECTS every password — confirms we are not
	// falling through to PasswordCallback. If NoClientAuth weren't
	// flipping, the test would fail with handshake error.
	auth := hostauth.StubAuth{Want: map[string]string{}}
	wsURL, hostKey := newTestSSHServerOpts(t, auth, true, true, true, true)

	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsConn, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	wsConn.SetReadLimit(-1)
	netConn := websocket.NetConn(context.Background(), wsConn, websocket.MessageBinary)
	cfg := &ssh.ClientConfig{
		User:            currentUser,
		Auth:            []ssh.AuthMethod{ssh.Password("ignored")},
		HostKeyCallback: ssh.FixedHostKey(hostKey.PublicKey()),
		Timeout:         5 * time.Second,
	}
	c, chans, reqs, err := ssh.NewClientConn(netConn, "test", cfg)
	if err != nil {
		t.Fatalf("ssh handshake should succeed via NoClientAuth path; got: %v", err)
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()
	out, err := session.Output("echo vouched")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(string(out), "vouched") {
		t.Errorf("exec stdout = %q, want 'vouched'", out)
	}
}

// TestSSHHandlerRejectsSubsystem — sftp / netconf subsystems are
// intentionally out of scope for v1. Confirms we don't accept them
// silently.
// TestSSHHandlerRejectsUnknownSubsystem verifies that subsystem requests
// for anything other than "sftp" are rejected. (The pre-SFTP test name
// here was `TestSSHHandlerRejectsSubsystem` and asserted that "sftp" was
// rejected — that's now wrong: sftp is accepted by default. Use a name
// the server has never heard of, like the historical "netconf".)
func TestSSHHandlerRejectsUnknownSubsystem(t *testing.T) {
	currentUser, err := hostauth.CurrentUser()
	if err != nil || currentUser == "" {
		t.Skip("cannot determine current OS user")
	}
	auth := hostauth.StubAuth{Want: map[string]string{currentUser: "secret"}}
	wsURL, hostKey := newTestSSHServer(t, auth)

	client, err := dialSSHOverWS(t, wsURL, hostKey, currentUser, "secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()

	if err := session.RequestSubsystem("netconf"); err == nil {
		t.Fatal("expected subsystem 'netconf' to be rejected")
	}
}

// TestSSHHandlerRejectsSFTPWhenDisabled asserts that the SFTP subsystem
// is rejected when the per-config toggle is off — operators who want to
// force `scp -O` (exec channel) can do so by flipping SFTPEnabled.
func TestSSHHandlerRejectsSFTPWhenDisabled(t *testing.T) {
	currentUser, err := hostauth.CurrentUser()
	if err != nil || currentUser == "" {
		t.Skip("cannot determine current OS user")
	}
	auth := hostauth.StubAuth{Want: map[string]string{currentUser: "secret"}}
	wsURL, hostKey := newTestSSHServerOpts(t, auth, false, true, true, false /*sftpEnabled*/)

	client, err := dialSSHOverWS(t, wsURL, hostKey, currentUser, "secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()

	if err := session.RequestSubsystem("sftp"); err == nil {
		t.Fatal("expected sftp subsystem to be rejected when SFTPEnabled=false")
	}
}

// TestSSHHandlerSFTPHappyPath opens the sftp subsystem, lists the temp
// directory, writes a small file via SFTP, reads it back, and asserts
// the contents. End-to-end proof that scp (which rides sftp by default
// on modern openssh) and sftp both work via outpost ssh-proxy.
func TestSSHHandlerSFTPHappyPath(t *testing.T) {
	currentUser, err := hostauth.CurrentUser()
	if err != nil || currentUser == "" {
		t.Skip("cannot determine current OS user")
	}
	auth := hostauth.StubAuth{Want: map[string]string{currentUser: "secret"}}
	wsURL, hostKey := newTestSSHServer(t, auth)

	client, err := dialSSHOverWS(t, wsURL, hostKey, currentUser, "secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	sc, err := sftp.NewClient(client)
	if err != nil {
		t.Fatalf("sftp client: %v", err)
	}
	defer sc.Close()

	tmp := t.TempDir()
	path := tmp + "/hello.txt"
	want := "hello from sftp"

	f, err := sc.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.Write([]byte(want)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rf, err := sc.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rf.Close()
	got, err := io.ReadAll(rf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestSSH_DirectTCPIP_LoopbackHappyPath stands up a tiny HTTP server on
// 127.0.0.1, then has the SSH client open a direct-tcpip channel at
// that host:port (the bytes the `ssh -L` client side puts on the wire)
// and round-trips a GET through it. Proves stock `ssh -L` works.
func TestSSH_DirectTCPIP_LoopbackHappyPath(t *testing.T) {
	currentUser, err := hostauth.CurrentUser()
	if err != nil || currentUser == "" {
		t.Skip("no current user; skipping")
	}
	auth := hostauth.StubAuth{Want: map[string]string{currentUser: "secret"}}
	wsURL, hostKey := newTestSSHServer(t, auth)

	// Loopback target: a stdlib HTTP server returning "pong".
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "pong")
	})
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	upstreamURL, _ := url.Parse(upstream.URL)
	host, portStr, _ := net.SplitHostPort(upstreamURL.Host)
	port, _ := strconv.Atoi(portStr)

	client, err := dialSSHOverWS(t, wsURL, hostKey, currentUser, "secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// crypto/ssh exposes "direct-tcpip" via Dial — exactly the path
	// `ssh -L` clients use under the hood.
	conn, err := client.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("direct-tcpip dial: %v", err)
	}
	defer conn.Close()

	// One HTTP/1.1 request over the SSH-channel-backed net.Conn.
	if _, err := io.WriteString(conn,
		"GET /ping HTTP/1.1\r\nHost: "+upstreamURL.Host+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	body, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "pong") {
		t.Errorf("expected body to contain 'pong', got %q", string(body))
	}
}

// TestSSH_DirectTCPIP_NonLoopbackRejected proves the loopback allowlist
// is doing its job — a channel-open at a non-loopback host gets
// rejected with Prohibited (no dial, no listener probe).
func TestSSH_DirectTCPIP_NonLoopbackRejected(t *testing.T) {
	currentUser, err := hostauth.CurrentUser()
	if err != nil || currentUser == "" {
		t.Skip("no current user; skipping")
	}
	auth := hostauth.StubAuth{Want: map[string]string{currentUser: "secret"}}
	wsURL, hostKey := newTestSSHServer(t, auth)

	client, err := dialSSHOverWS(t, wsURL, hostKey, currentUser, "secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	_, err = client.Dial("tcp", "10.0.0.1:80")
	if err == nil {
		t.Fatal("expected non-loopback direct-tcpip to be rejected")
	}
	// crypto/ssh wraps the reject reason in the error message.
	if !strings.Contains(err.Error(), "loopback") &&
		!strings.Contains(strings.ToLower(err.Error()), "prohibit") {
		t.Errorf("expected rejection message to mention loopback/prohibit, got %v", err)
	}
}

// TestAllowTCPIPForwardBind exercises the bind-address allowlist used by
// `tcpip-forward` (ssh -R). Loopback only; empty string ("") matches
// openssh's default-to-127.0.0.1 behavior.
func TestAllowTCPIPForwardBind(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"localhost", true},
		{"LOCALHOST", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"  127.0.0.1  ", true},
		{"0.0.0.0", false},
		{"192.168.1.10", false},
		{"example.com", false},
	}
	for _, tc := range cases {
		if got := allowTCPIPForwardBind(tc.in); got != tc.want {
			t.Errorf("allowTCPIPForwardBind(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestSSH_RemoteForward_HappyPath spins up a `tcpip-forward` listener via
// the SSH client's Listen(), dials the bound port from the test process,
// and round-trips bytes through the resulting `forwarded-tcpip` channel.
// This is the integration test for ssh -R end-to-end.
func TestSSH_RemoteForward_HappyPath(t *testing.T) {
	currentUser, err := hostauth.CurrentUser()
	if err != nil || currentUser == "" {
		t.Skip("no current user; skipping")
	}
	auth := hostauth.StubAuth{Want: map[string]string{currentUser: "secret"}}
	wsURL, hostKey := newTestSSHServer(t, auth)

	client, err := dialSSHOverWS(t, wsURL, hostKey, currentUser, "secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// Ask the agent to bind a loopback listener and push accepts back.
	ln, err := client.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("client.Listen: %v", err)
	}
	defer ln.Close()

	// Echo accepted connections back so a write→read round-trip is visible.
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	// Dial the bound port from the test process (it's loopback on this host).
	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d := net.Dialer{}
	c, err := d.DialContext(dialCtx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial bound port: %v", err)
	}
	defer c.Close()

	payload := []byte("hello-forwarded-tcpip\n")
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("echo mismatch: got %q, want %q", got, payload)
	}
}

// TestSSH_RemoteForward_Disabled — when the toggle is off, the
// tcpip-forward request must be rejected at the global-request layer
// (client.Listen returns an error).
func TestSSH_RemoteForward_Disabled(t *testing.T) {
	currentUser, err := hostauth.CurrentUser()
	if err != nil || currentUser == "" {
		t.Skip("no current user; skipping")
	}
	auth := hostauth.StubAuth{Want: map[string]string{currentUser: "secret"}}
	// allowRemoteForward=false (4th bool); rest match the happy path.
	wsURL, hostKey := newTestSSHServerOpts(t, auth, false, true, false, true)

	client, err := dialSSHOverWS(t, wsURL, hostKey, currentUser, "secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if _, err := client.Listen("tcp", "127.0.0.1:0"); err == nil {
		t.Fatal("expected tcpip-forward to be rejected when remote-forward disabled")
	}
}

// TestSSH_RemoteForward_NonLoopbackRefused proves the bind-address
// allowlist defends against 0.0.0.0 / public-interface binds even when
// the toggle is on.
func TestSSH_RemoteForward_NonLoopbackRefused(t *testing.T) {
	currentUser, err := hostauth.CurrentUser()
	if err != nil || currentUser == "" {
		t.Skip("no current user; skipping")
	}
	auth := hostauth.StubAuth{Want: map[string]string{currentUser: "secret"}}
	wsURL, hostKey := newTestSSHServer(t, auth)

	client, err := dialSSHOverWS(t, wsURL, hostKey, currentUser, "secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if _, err := client.Listen("tcp", "0.0.0.0:0"); err == nil {
		t.Fatal("expected tcpip-forward to be rejected for 0.0.0.0 bind")
	}
}

// TestSSH_DirectTCPIP_Disabled proves the agent-config toggle is wired
// — when the flag is false, even loopback targets are refused. Lets
// an operator opt out without recompiling.
func TestSSH_DirectTCPIP_Disabled(t *testing.T) {
	currentUser, err := hostauth.CurrentUser()
	if err != nil || currentUser == "" {
		t.Skip("no current user; skipping")
	}
	auth := hostauth.StubAuth{Want: map[string]string{currentUser: "secret"}}
	// allowLocalForward=false — the only difference from the happy path.
	wsURL, hostKey := newTestSSHServerOpts(t, auth, false, false, true, true)

	client, err := dialSSHOverWS(t, wsURL, hostKey, currentUser, "secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	_, err = client.Dial("tcp", "127.0.0.1:1")
	if err == nil {
		t.Fatal("expected direct-tcpip to be rejected when forwarding disabled")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "disabled") &&
		!strings.Contains(strings.ToLower(err.Error()), "prohibit") {
		t.Errorf("expected rejection to mention disabled/prohibit, got %v", err)
	}
}
