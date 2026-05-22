package agent

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// newTestRouter is a tiny gin engine that mounts only the registry's
// /app/:name/*p proxy route. Used by the socket-proxy test below.
func newTestRouter(reg *AppRegistry) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Any("/app/:name", reg.handler())
	r.Any("/app/:name/*p", reg.handler())
	return r
}

// TestRegisterFromConfig — happy path plus validation: bad scheme/port
// must error, disabled entries must be silently skipped (so the admin UI
// can keep them in the persisted list without serving them).
func TestRegisterFromConfig(t *testing.T) {
	reg := NewAppRegistry()

	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "alpha", Scheme: "http", Host: "127.0.0.1", Port: 9000, Enabled: true,
	}); err != nil {
		t.Fatalf("happy: %v", err)
	}
	if target := reg.LookupTarget("alpha"); target == nil || target.Host != "127.0.0.1:9000" {
		t.Errorf("registered target = %v", target)
	}

	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "beta", Scheme: "http", Port: 8000, Enabled: false,
	}); err != nil {
		t.Errorf("disabled entry should be skipped silently, got error: %v", err)
	}
	if reg.LookupTarget("beta") != nil {
		t.Error("disabled app should not be in the live registry")
	}

	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "gamma", Scheme: "ftp", Port: 21, Enabled: true,
	}); err == nil {
		t.Error("expected error on unsupported scheme")
	}

	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "delta", Scheme: "http", Port: 0, Enabled: true,
	}); err == nil {
		t.Error("expected error on zero port")
	}
}

// TestRegisterWithRole_DefaultsAndValidation: empty role defaults to
// "user"; a recognised role is preserved verbatim; an unrecognised role
// errors out at register time so misconfigured apps never reach the cloud.
func TestRegisterWithRole_DefaultsAndValidation(t *testing.T) {
	reg := NewAppRegistry()

	if err := reg.RegisterWithRole("a", "http://127.0.0.1:9000", ""); err != nil {
		t.Fatalf("empty role: %v", err)
	}
	if err := reg.RegisterWithRole("b", "http://127.0.0.1:9001", "admin"); err != nil {
		t.Fatalf("admin role: %v", err)
	}
	if err := reg.RegisterWithRole("c", "http://127.0.0.1:9002", "root"); err == nil {
		t.Fatalf("unrecognised role should error")
	}

	got := map[string]string{}
	for _, e := range reg.Entries() {
		got[e.Name] = e.Role
	}
	if got["a"] != "user" {
		t.Errorf("empty role should default to user, got %q", got["a"])
	}
	if got["b"] != "admin" {
		t.Errorf("admin role should be preserved, got %q", got["b"])
	}
	if _, ok := got["c"]; ok {
		t.Errorf("unrecognised role should not have been registered")
	}
}

// TestRegisterFromConfig_RolePropagates: AppConfig.Role flows into the
// registry's Entries() output so /apps publishes the owner's declarations.
func TestRegisterFromConfig_RolePropagates(t *testing.T) {
	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "jupyter", Scheme: "http", Port: 8888, Enabled: true, Role: "admin",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, e := range reg.Entries() {
		if e.Name == "jupyter" && e.Role != "admin" {
			t.Errorf("role on AppConfig should propagate, got %q", e.Role)
		}
	}
}

// TestRegisterFromConfig_UnixSocketProxy: a unix-scheme app dials the
// configured socket regardless of the request URL. We stand up a tiny
// HTTP server on a unix socket in a tempdir, register it, and make sure
// `/app/<name>/ping` actually reaches it.
func TestRegisterFromConfig_UnixSocketProxy(t *testing.T) {
	// macOS caps sun_path at 104 chars, so the default t.TempDir() under
	// /var/folders/... is too long. Use /tmp directly.
	sockDir, err := os.MkdirTemp("/tmp", "outpost-sock-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "podman.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "pong-"+r.Header.Get("X-Test"))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name:    "podman",
		Scheme:  "unix",
		Socket:  sockPath,
		Enabled: true,
		Role:    "admin",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if target := reg.LookupTarget("podman"); target == nil || target.Host != "socket" {
		t.Errorf("socket-backed target host = %v (want synthetic 'socket')", target)
	}
	roles := map[string]string{}
	for _, e := range reg.Entries() {
		roles[e.Name] = e.Role
	}
	if roles["podman"] != "admin" {
		t.Errorf("role on socket app should propagate, got %q", roles["podman"])
	}

	// Stand up the registry behind a real httptest.Server so the full
	// proxy path runs (gin's responseWriter.CloseNotify needs a real
	// http.ResponseWriter — httptest.ResponseRecorder doesn't implement
	// http.CloseNotifier).
	front := httptest.NewServer(newTestRouter(reg))
	t.Cleanup(front.Close)

	req, _ := http.NewRequest("GET", front.URL+"/app/podman/ping", nil)
	req.Header.Set("X-Test", "ok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); got != "pong-ok" {
		t.Errorf("proxy body = %q (want pong-ok)", got)
	}
}

// TestRegisterFromConfig_SocketSchemeRequiresSocketField: scheme=unix
// without a socket path must error at register time rather than silently
// proxying to nowhere.
func TestRegisterFromConfig_SocketSchemeRequiresSocketField(t *testing.T) {
	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "podman", Scheme: "unix", Enabled: true,
	}); err == nil {
		t.Error("expected error when unix scheme has no socket")
	}
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "podman", Scheme: "npipe", Enabled: true,
	}); err == nil {
		t.Error("expected error when npipe scheme has no socket")
	}
}

// TestUnregister removes the entry from the registry; subsequent lookups
// must return nil, and a re-Register with a different target must take.
func TestUnregister(t *testing.T) {
	reg := NewAppRegistry()
	if err := reg.Register("ycode", "http://127.0.0.1:8765"); err != nil {
		t.Fatal(err)
	}
	reg.Unregister("ycode")
	if reg.LookupTarget("ycode") != nil {
		t.Error("Unregister did not clear the entry")
	}
	// Unregister of a missing entry is a no-op.
	reg.Unregister("ycode")
	// Re-register with a different host should stick.
	if err := reg.Register("ycode", "http://127.0.0.1:9999"); err != nil {
		t.Fatal(err)
	}
	if reg.LookupTarget("ycode").Host != "127.0.0.1:9999" {
		t.Errorf("re-register did not take")
	}
}

// TestRegisterFromConfig_XForwardedHeaders confirms the reverse proxy
// sets X-Forwarded-* on the outbound request so well-behaved web apps
// (Grafana et al.) can construct correct absolute URLs without an
// outpost-side path rewrite. Defaults apply only when the inbound
// request didn't already carry the header (cloud-supplied values win).
func TestRegisterFromConfig_XForwardedHeaders(t *testing.T) {
	type seen struct {
		host, proto, prefix, forFor string
	}
	var got seen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = seen{
			host:   r.Header.Get("X-Forwarded-Host"),
			proto:  r.Header.Get("X-Forwarded-Proto"),
			prefix: r.Header.Get("X-Forwarded-Prefix"),
			forFor: r.Header.Get("X-Forwarded-For"),
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	uhost, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)

	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "grafana", Scheme: "http", Host: uhost, Port: port, Enabled: true,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(newTestRouter(reg))
	defer srv.Close()

	t.Run("defaults when no cloud headers", func(t *testing.T) {
		got = seen{}
		req, _ := http.NewRequest("GET", srv.URL+"/app/grafana/dashboards", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		_ = resp.Body.Close()
		// httptest.NewServer listens on 127.0.0.1:<random>; that's the
		// Host the proxy should default to.
		if got.host == "" {
			t.Errorf("X-Forwarded-Host not set; got empty")
		}
		if got.proto != "http" {
			t.Errorf("X-Forwarded-Proto = %q, want http", got.proto)
		}
		if got.prefix != "/app/grafana" {
			t.Errorf("X-Forwarded-Prefix = %q, want /app/grafana", got.prefix)
		}
		if got.forFor == "" {
			t.Errorf("X-Forwarded-For not set")
		}
	})

	t.Run("cloud-supplied values win", func(t *testing.T) {
		got = seen{}
		req, _ := http.NewRequest("GET", srv.URL+"/app/grafana/dashboards", nil)
		req.Header.Set("X-Forwarded-Host", "ai.dhnt.io")
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Prefix", "/h/novicortex/app/grafana")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		_ = resp.Body.Close()
		if got.host != "ai.dhnt.io" {
			t.Errorf("X-Forwarded-Host = %q, want ai.dhnt.io", got.host)
		}
		if got.proto != "https" {
			t.Errorf("X-Forwarded-Proto = %q, want https", got.proto)
		}
		if got.prefix != "/h/novicortex/app/grafana" {
			t.Errorf("X-Forwarded-Prefix = %q, want /h/novicortex/app/grafana", got.prefix)
		}
	})
}

// TestRegisterFromConfig_TCPBridge spins up a tiny upstream TCP echo,
// registers it as a tcp-scheme app, dials a WebSocket against the gin
// router, and confirms the bridge byte-splices both directions. This is
// the same code path /app/<name>/ runs in production for ssh/postgres.
func TestRegisterFromConfig_TCPBridge(t *testing.T) {
	// Upstream: trivial echo server on a random loopback port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	go func() {
		for {
			c, aerr := l.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()
	host, portStr, _ := net.SplitHostPort(l.Addr().String())
	port, _ := strconv.Atoi(portStr)

	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "echo", Scheme: "tcp", Host: host, Port: port, Enabled: true, Role: "user",
	}); err != nil {
		t.Fatalf("register tcp: %v", err)
	}
	if got := reg.LookupTCP("echo"); got == "" {
		t.Fatalf("LookupTCP returned empty")
	}
	if reg.LookupTarget("echo") != nil {
		t.Fatalf("tcp app should not have a URL target")
	}

	srv := httptest.NewServer(newTestRouter(reg))
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http", "ws", 1) + "/app/echo/"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "done")

	// Treat the WS as a stream and confirm the echo round-trips.
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	defer conn.Close()
	want := []byte("hello tcp bridge")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("echo got %q, want %q", got, want)
	}
}

// TestRegisterFromConfig_TCPModeSwap confirms that re-registering a name
// under a different scheme (http→tcp or vice versa) cleanly replaces the
// previous mode instead of leaving stale state behind.
func TestRegisterFromConfig_TCPModeSwap(t *testing.T) {
	reg := NewAppRegistry()
	// Start http.
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "x", Scheme: "http", Host: "127.0.0.1", Port: 9000, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if reg.LookupTarget("x") == nil {
		t.Fatal("http target should be set")
	}
	// Swap to tcp on the same name.
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "x", Scheme: "tcp", Host: "127.0.0.1", Port: 22, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if reg.LookupTarget("x") != nil {
		t.Errorf("http target should have been cleared on tcp register")
	}
	if reg.LookupTCP("x") == "" {
		t.Errorf("tcp target should be set after swap")
	}
	// Swap back to http; tcp target must clear.
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "x", Scheme: "http", Host: "127.0.0.1", Port: 8080, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if reg.LookupTCP("x") != "" {
		t.Errorf("tcp target should have been cleared on http register")
	}
}
