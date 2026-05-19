package agent

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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
