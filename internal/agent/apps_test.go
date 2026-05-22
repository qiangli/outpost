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

// TestRegisterWithMeta_Defaults: Register defaults to require_login=true;
// RegisterWithMeta carries through the explicit flag.
func TestRegisterWithMeta_Defaults(t *testing.T) {
	reg := NewAppRegistry()

	if err := reg.Register("a", "http://127.0.0.1:9000"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.RegisterWithMeta("b", "http://127.0.0.1:9001", AppMeta{RequireLogin: false}); err != nil {
		t.Fatalf("RegisterWithMeta: %v", err)
	}

	got := map[string]bool{}
	for _, e := range reg.Entries() {
		got[e.Name] = e.RequireLogin
	}
	if got["a"] != true {
		t.Errorf("Register default require_login should be true, got %v", got["a"])
	}
	if got["b"] != false {
		t.Errorf("RegisterWithMeta require_login=false should propagate, got %v", got["b"])
	}
}

// TestRegisterFromConfig_RequireLoginPropagates: AppConfig.RequireLogin
// flows into the registry's Entries() output so /apps publishes the
// owner's declarations to the cloud.
func TestRegisterFromConfig_RequireLoginPropagates(t *testing.T) {
	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "jupyter", Scheme: "http", Port: 8888, Enabled: true, RequireLogin: true,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, e := range reg.Entries() {
		if e.Name == "jupyter" && !e.RequireLogin {
			t.Errorf("require_login on AppConfig should propagate, got %v", e.RequireLogin)
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
		Name:         "podman",
		Scheme:       "unix",
		Socket:       sockPath,
		Enabled:      true,
		RequireLogin: true,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if target := reg.LookupTarget("podman"); target == nil || target.Host != "socket" {
		t.Errorf("socket-backed target host = %v (want synthetic 'socket')", target)
	}
	gotReqLogin := map[string]bool{}
	for _, e := range reg.Entries() {
		gotReqLogin[e.Name] = e.RequireLogin
	}
	if !gotReqLogin["podman"] {
		t.Errorf("require_login on socket app should propagate, got %v", gotReqLogin["podman"])
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

// TestProxyTo_RequireLogin_BlocksCloudWithoutPeriscopeRole confirms the
// cloud-side gate: when an app requires login, a request coming through
// cloudbox (X-Forwarded-Prefix present) but without X-Periscope-Role
// gets 403 and the upstream is never dialed.
func TestProxyTo_RequireLogin_BlocksCloudWithoutPeriscopeRole(t *testing.T) {
	var upstreamHit int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	uhost, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)

	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "guarded", Scheme: "http", Host: uhost, Port: port, Enabled: true, RequireLogin: true,
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newTestRouter(reg))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/app/guarded/x", nil)
	req.Header.Set("X-Forwarded-Prefix", "/h/dragon/app/guarded")
	// NO X-Periscope-Role — simulates an un-elevated cloud caller.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if upstreamHit != 0 {
		t.Fatalf("upstream was hit %d times, want 0", upstreamHit)
	}
}

// TestProxyTo_RequireLogin_AllowsCloudWithPeriscopeRole confirms the
// same setup proceeds normally once cloudbox has stamped X-Periscope-Role.
func TestProxyTo_RequireLogin_AllowsCloudWithPeriscopeRole(t *testing.T) {
	var upstreamHit int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	uhost, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)

	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "guarded", Scheme: "http", Host: uhost, Port: port, Enabled: true, RequireLogin: true,
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newTestRouter(reg))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/app/guarded/x", nil)
	req.Header.Set("X-Forwarded-Prefix", "/h/dragon/app/guarded")
	req.Header.Set("X-Periscope-Role", "user")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if upstreamHit != 1 {
		t.Fatalf("upstream was hit %d times, want 1", upstreamHit)
	}
}

// TestProxyTo_RequireLogin_AllowsDirectLoopback confirms that the
// require_login gate is cloud-side-only: a direct loopback request
// (no X-Forwarded-Prefix, e.g. the admin UI subpath route or local
// tooling) passes even without X-Periscope-Role.
func TestProxyTo_RequireLogin_AllowsDirectLoopback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	uhost, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)

	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "guarded", Scheme: "http", Host: uhost, Port: port, Enabled: true, RequireLogin: true,
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newTestRouter(reg))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/app/guarded/x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no X-Forwarded-Prefix → gate skipped)", resp.StatusCode)
	}
}

// TestProxyTo_LANOnlyPaths_BlocksCloud confirms LAN-only paths 404 when
// reached via cloudbox.
func TestProxyTo_LANOnlyPaths_BlocksCloud(t *testing.T) {
	var upstreamHit int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	uhost, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)

	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "class", Scheme: "http", Host: uhost, Port: port, Enabled: true,
		// Not require_login here so we isolate the LAN-only gate.
		LANOnlyPaths: []string{"/kiosk"},
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newTestRouter(reg))
	defer srv.Close()

	for _, tc := range []struct {
		name, path string
		want       int
	}{
		{"exact match blocked", "/app/class/kiosk", http.StatusNotFound},
		{"deeper match blocked", "/app/class/kiosk/check-in", http.StatusNotFound},
		{"segment boundary respected", "/app/class/kiosks-of-truth", http.StatusOK},
		{"other path allowed", "/app/class/admin", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", srv.URL+tc.path, nil)
			req.Header.Set("X-Forwarded-Prefix", "/h/dragon/app/class")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// TestProxyTo_LANOnlyPaths_AllowsDirect confirms LAN-only paths pass
// through on direct loopback (no X-Forwarded-Prefix) — that's the
// kiosk-on-LAN use case.
func TestProxyTo_LANOnlyPaths_AllowsDirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	uhost, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)

	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "class", Scheme: "http", Host: uhost, Port: port, Enabled: true,
		LANOnlyPaths: []string{"/kiosk"},
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newTestRouter(reg))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/app/class/kiosk/check-in")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("direct LAN access blocked: %d (want 200)", resp.StatusCode)
	}
}

// TestRegisterFromConfig_IndexPathRoundTrip confirms IndexPath flows
// through the registry into the Entries() output (= published via
// /apps). The proxy itself ignores it — it's a cloud-SPA UX hint.
func TestRegisterFromConfig_IndexPathRoundTrip(t *testing.T) {
	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "class-admin", Scheme: "http", Host: "127.0.0.1", Port: 8080,
		Enabled: true, RequireLogin: true, IndexPath: "/admin",
	}); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range reg.Entries() {
		if e.Name == "class-admin" {
			found = true
			if e.IndexPath != "/admin" {
				t.Errorf("IndexPath = %q, want /admin", e.IndexPath)
			}
			if !e.RequireLogin {
				t.Errorf("RequireLogin lost in registration")
			}
		}
	}
	if !found {
		t.Fatal("class-admin not registered")
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
