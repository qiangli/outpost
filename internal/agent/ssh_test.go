package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh"

	"github.com/qiangli/outpost/internal/agent/hostauth"
)

// newTestSSHServer spins up an httptest server with the SSH handler
// mounted at /ssh and returns its ws:// URL.
func newTestSSHServer(t *testing.T, auth hostauth.Authenticator) (wsURL string, hostKey ssh.Signer) {
	t.Helper()
	return newTestSSHServerOpts(t, auth, false)
}

// newTestSSHServerOpts is the parameterized form. cloudboxStamps inserts
// an X-Periscope-Role: admin header on every request via gin middleware,
// simulating cloudbox's SSHProxy vouching for the caller.
func newTestSSHServerOpts(t *testing.T, auth hostauth.Authenticator, cloudboxStamps bool) (wsURL string, hostKey ssh.Signer) {
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

	engine.GET("/ssh", sshHandler(signer, auth, ""))

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
	wsURL, hostKey := newTestSSHServerOpts(t, auth, true)

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
func TestSSHHandlerRejectsSubsystem(t *testing.T) {
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

	if err := session.RequestSubsystem("sftp"); err == nil {
		t.Fatal("expected subsystem 'sftp' to be rejected")
	}
}
