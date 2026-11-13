package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh"
)

// ingressFixture serves one RegisterRoutes engine on a loopback TCP listener
// (the local door) and on the private tunnel-ingress socket.
type ingressFixture struct {
	apps     *AppRegistry
	localURL string
	ingress  string // unix socket path
}

func newIngressFixture(t *testing.T) *ingressFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	apps := NewAppRegistry()
	engine := gin.New()
	RegisterRoutes(engine.Group("/"), Deps{Apps: apps, SSHHostKey: signer})
	local := httptest.NewServer(engine)
	t.Cleanup(local.Close)
	ln, path, cleanup, err := ListenTunnelIngress()
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	srv := &http.Server{Handler: engine, ConnContext: TunnelIngressConnContext}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); cleanup() })
	return &ingressFixture{apps: apps, localURL: local.URL, ingress: path}
}

// dialWS opens a WebSocket on the local door (viaIngress=false) or the tunnel
// ingress. It returns the conn (nil on refusal) and the HTTP status.
func (f *ingressFixture) dialWS(t *testing.T, viaIngress bool, path string, hdr http.Header) (*websocket.Conn, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	opts := &websocket.DialOptions{HTTPHeader: hdr}
	u := strings.Replace(f.localURL, "http", "ws", 1) + path
	if viaIngress {
		opts.HTTPClient = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", f.ingress)
		}}}
		u = "ws://ingress" + path
	}
	c, resp, err := websocket.Dial(ctx, u, opts)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	if err != nil {
		return nil, status
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c, status
}

// sshNoAuth reports whether an SSH handshake with NO client auth succeeds.
func (f *ingressFixture) sshNoAuth(t *testing.T, viaIngress bool, role string) bool {
	t.Helper()
	hdr := http.Header{}
	if role != "" {
		hdr.Set("X-Periscope-Role", role)
	}
	c, status := f.dialWS(t, viaIngress, "/ssh", hdr)
	if c == nil {
		t.Fatalf("/ssh upgrade refused (status %d)", status)
	}
	nc := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	defer nc.Close()
	cfg := &ssh.ClientConfig{User: "nobody", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second}
	sc, _, _, err := ssh.NewClientConn(nc, "outpost", cfg)
	if err != nil {
		return false
	}
	_ = sc.Close()
	return true
}

func httpStatus(t *testing.T, method, url string) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// Before the fix: an unauthenticated WebSocket upgrade of /shell on the
// loopback listener got a PTY (101), and a forged X-Periscope-Role: admin
// on /ssh completed an SSH handshake with no client auth.
func TestLoopbackRoutesRefusedOffTunnelIngress(t *testing.T) {
	f := newIngressFixture(t)
	if c, status := f.dialWS(t, false, "/shell", nil); c != nil || status != http.StatusForbidden {
		t.Errorf("local /shell upgrade: conn=%v status=%d, want refused 403", c != nil, status)
	}
	if c, status := f.dialWS(t, false, "/desktop", nil); c != nil || status != http.StatusForbidden {
		t.Errorf("local /desktop upgrade: conn=%v status=%d, want refused 403", c != nil, status)
	}
	if got := httpStatus(t, http.MethodGet, f.localURL+"/clipboard"); got != http.StatusForbidden {
		t.Errorf("local GET /clipboard = %d, want 403", got)
	}
	if f.sshNoAuth(t, false, "admin") {
		t.Error("local /ssh with a forged X-Periscope-Role: admin completed a no-auth SSH handshake")
	}
	if f.sshNoAuth(t, false, "") {
		t.Error("local /ssh without a role completed a no-auth SSH handshake")
	}
	// A browser origin is refused on the local door even for /ssh.
	hdr := http.Header{"Origin": []string{"http://evil.example"}}
	if c, status := f.dialWS(t, false, "/ssh", hdr); c != nil || status != http.StatusForbidden {
		t.Errorf("local cross-origin /ssh upgrade: conn=%v status=%d, want 403", c != nil, status)
	}
	// Healthz stays public.
	if got := httpStatus(t, http.MethodGet, f.localURL+"/healthz"); got != http.StatusOK {
		t.Errorf("local GET /healthz = %d, want 200", got)
	}
}

func TestTunnelIngressStillServesCloudRoutes(t *testing.T) {
	f := newIngressFixture(t)
	if c, status := f.dialWS(t, true, "/shell", nil); c == nil {
		t.Errorf("ingress /shell upgrade refused (status %d)", status)
	}
	if !f.sshNoAuth(t, true, "admin") {
		t.Error("ingress /ssh with cloudbox's X-Periscope-Role: admin did not skip the password")
	}
	if f.sshNoAuth(t, true, "") {
		t.Error("ingress /ssh with no role skipped the password")
	}
}

// The escape hatch reproduces the old loopback trust for non-browser
// clients; a cross-origin browser request is still refused.
func TestTrustLoopbackEscapeHatch(t *testing.T) {
	f := newIngressFixture(t)
	f.apps.SetTrustLoopback(true)
	if c, status := f.dialWS(t, false, "/shell", nil); c == nil {
		t.Errorf("escape hatch: local /shell upgrade refused (status %d)", status)
	}
	if !f.sshNoAuth(t, false, "admin") {
		t.Error("escape hatch: local /ssh role header not honored")
	}
	hdr := http.Header{"Origin": []string{"http://evil.example"}}
	if c, status := f.dialWS(t, false, "/shell", hdr); c != nil || status != http.StatusForbidden {
		t.Errorf("escape hatch: cross-origin /shell: conn=%v status=%d, want 403", c != nil, status)
	}
}

func TestLocalOriginGuardRebinding(t *testing.T) {
	for _, tc := range []struct {
		host, origin string
		ok           bool
	}{
		{"127.0.0.1:9", "", true},
		{"127.0.0.1:9", "http://127.0.0.1:9", true},
		{"localhost:9", "http://localhost:9", true},
		{"127.0.0.1:9", "http://evil.example", false},
		// DNS rebinding: Origin and Host agree on the attacker's name.
		{"evil.example:9", "http://evil.example:9", false},
	} {
		gin.SetMode(gin.TestMode)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Request.Host = tc.host
		if tc.origin != "" {
			c.Request.Header.Set("Origin", tc.origin)
		}
		localOriginGuard(c)
		if got := !c.IsAborted(); got != tc.ok {
			t.Errorf("host=%s origin=%q allowed=%v, want %v", tc.host, tc.origin, got, tc.ok)
		}
	}
}
