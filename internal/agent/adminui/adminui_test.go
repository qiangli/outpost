package adminui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
)

// newTestServer builds a Server wired to a temp config path and a stub
// authenticator — no listener, no real OS PAM calls. The returned engine
// is what tests drive via httptest.
func newTestServer(t *testing.T, configPath string, want map[string]string, restartCalls *atomic.Int32) *Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	eng := gin.New()
	s := &Server{
		deps: Deps{
			ConfigPath: configPath,
			Auth:       hostauth.StubAuth{Want: want},
			Apps:       agent.NewAppRegistry(),
			Restart: func() {
				if restartCalls != nil {
					restartCalls.Add(1)
				}
			},
		},
		engine:       eng,
		sessions:     newSessionStore(time.Minute, nil),
		loginRL:      newLoginLimiter(50, time.Millisecond),
		loopbackOnly: true,
		detector:     agent.NewBuiltinDetector(0),
	}
	s.registerRoutes()
	return s
}

func doJSON(s *Server, method, path string, body any, cookie string) *httptest.ResponseRecorder {
	var bs []byte
	if body != nil {
		bs, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bs))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	}
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	return w
}

// TestGateOpenOnFirstRun — when the config file doesn't exist yet, the
// admin API must be reachable without a cookie. Otherwise a fresh
// install couldn't reach the pairing form.
func TestGateOpenOnFirstRun(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	s := newTestServer(t, configPath, nil, nil)
	w := doJSON(s, http.MethodGet, "/api/status", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status on first run = %d, want 200 (open gate). Body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["configured"] != false {
		t.Errorf("configured = %v, want false", got["configured"])
	}
}

// TestGateClosedAfterConfig — once a config file exists, the gate must
// reject API calls without a valid session cookie.
func TestGateClosedAfterConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(configPath, &conf.FileConfig{AgentName: "x", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, configPath, nil, nil)
	w := doJSON(s, http.MethodGet, "/api/status", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("gate didn't engage: status %d, want 401. Body: %s", w.Code, w.Body.String())
	}
}

// TestLoginMintsCookie — successful POST /api/login lets subsequent calls
// in. Wrong password is rejected. Imposter username (≠ running OS user)
// is rejected even with the right password.
func TestLoginMintsCookie(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(configPath, &conf.FileConfig{AgentName: "x", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	user, err := hostauth.CurrentUser()
	if err != nil || user == "" {
		t.Skip("cannot determine OS user")
	}
	s := newTestServer(t, configPath, map[string]string{user: "secret"}, nil)

	// Bad password.
	w := doJSON(s, http.MethodPost, "/api/login", map[string]string{"user": user, "password": "nope"}, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("bad password status = %d, want 401", w.Code)
	}

	// Imposter user — right password, wrong username.
	w = doJSON(s, http.MethodPost, "/api/login", map[string]string{"user": user + "-imposter", "password": "secret"}, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("imposter status = %d, want 401", w.Code)
	}

	// Right credentials.
	w = doJSON(s, http.MethodPost, "/api/login", map[string]string{"user": user, "password": "secret"}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	cookie := extractCookie(w, cookieName)
	if cookie == "" {
		t.Fatal("expected an outpost_admin cookie to be set on login")
	}

	// Cookie now grants access to gated endpoints.
	w = doJSON(s, http.MethodGet, "/api/config", nil, cookie)
	if w.Code != http.StatusOK {
		t.Errorf("gated GET /api/config with cookie = %d, want 200. Body: %s", w.Code, w.Body.String())
	}

	// Logout revokes the cookie.
	w = doJSON(s, http.MethodPost, "/api/logout", nil, cookie)
	if w.Code != http.StatusOK {
		t.Errorf("logout status = %d", w.Code)
	}
	w = doJSON(s, http.MethodGet, "/api/config", nil, cookie)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("post-logout call status = %d, want 401", w.Code)
	}
}

// TestConfigViewRedactsToken — the on-disk Token must never appear in any
// API response, even on the loopback admin surface. has_token reports
// presence so the UI can render "(set)" without seeing the secret.
func TestConfigViewRedactsToken(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(configPath, &conf.FileConfig{
		AgentName: "x", Token: "do-not-leak", ServerAddr: "edge.example.com", ServerPort: 443,
	}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, configPath, nil, nil)
	// Skip the gate by Validating a fresh cookie via the session store.
	cookie, _ := s.sessions.Mint("tester")

	w := doJSON(s, http.MethodGet, "/api/config", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d. Body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "do-not-leak") {
		t.Errorf("token leaked into /api/config response: %s", body)
	}
	var view map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if view["has_token"] != true {
		t.Errorf("has_token = %v, want true", view["has_token"])
	}
}

// TestAppCRUD walks add → list → update → delete through the admin API
// and confirms the live AppRegistry reflects each step.
func TestAppCRUD(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(configPath, &conf.FileConfig{AgentName: "x", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, configPath, nil, nil)
	cookie, _ := s.sessions.Mint("tester")

	// Add an enabled app.
	w := doJSON(s, http.MethodPost, "/api/apps", map[string]any{
		"name": "jupyter", "scheme": "http", "host": "127.0.0.1", "port": 8888, "enabled": true,
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/apps add = %d. Body: %s", w.Code, w.Body.String())
	}
	if s.deps.Apps.LookupTarget("jupyter") == nil {
		t.Error("live registry missing jupyter after add")
	}

	// List shows it.
	w = doJSON(s, http.MethodGet, "/api/apps", nil, cookie)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "jupyter") {
		t.Errorf("GET /api/apps = %d body=%s", w.Code, w.Body.String())
	}

	// Toggle disabled — the registry must drop it, but the persisted list
	// must still contain it (so the UI can show "off" and re-enable).
	w = doJSON(s, http.MethodPost, "/api/apps", map[string]any{
		"name": "jupyter", "scheme": "http", "host": "127.0.0.1", "port": 8888, "enabled": false,
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/apps update = %d. Body: %s", w.Code, w.Body.String())
	}
	if s.deps.Apps.LookupTarget("jupyter") != nil {
		t.Error("disabled app should be dropped from live registry")
	}
	fc, _ := conf.LoadFile(configPath)
	if len(fc.Apps) != 1 || fc.Apps[0].Enabled {
		t.Errorf("persisted apps after disable = %+v", fc.Apps)
	}

	// Delete clears both registry and persisted list.
	w = doJSON(s, http.MethodDelete, "/api/apps/jupyter", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d", w.Code)
	}
	fc, _ = conf.LoadFile(configPath)
	if len(fc.Apps) != 0 {
		t.Errorf("persisted apps after delete = %+v", fc.Apps)
	}
}

// TestAppValidation — invalid input must be rejected at the boundary so
// we never write a malformed AppConfig to disk.
func TestAppValidation(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(configPath, &conf.FileConfig{AgentName: "x", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, configPath, nil, nil)
	cookie, _ := s.sessions.Mint("tester")

	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing name", map[string]any{"scheme": "http", "port": 80, "enabled": true}},
		{"slash in name", map[string]any{"name": "a/b", "scheme": "http", "port": 80, "enabled": true}},
		{"bad scheme", map[string]any{"name": "x", "scheme": "ftp", "port": 80, "enabled": true}},
		{"zero port", map[string]any{"name": "x", "scheme": "http", "port": 0, "enabled": true}},
		{"huge port", map[string]any{"name": "x", "scheme": "http", "port": 999999, "enabled": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doJSON(s, http.MethodPost, "/api/apps", tc.body, cookie)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400. Body: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestBuiltinsToggleSchedulesRestart — flipping a built-in writes the
// new value to disk and (because routes mount at boot) schedules a
// restart. On first-run (no AgentName) no restart should fire.
func TestBuiltinsToggleSchedulesRestart(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(configPath, &conf.FileConfig{AgentName: "configured", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	var restartCount atomic.Int32
	s := newTestServer(t, configPath, nil, &restartCount)
	cookie, _ := s.sessions.Mint("tester")

	off := false
	w := doJSON(s, http.MethodPost, "/api/config/builtins", map[string]any{"shell": off}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d. Body: %s", w.Code, w.Body.String())
	}
	// scheduleRestart sleeps ~250ms; wait a bit longer than that.
	waitFor(t, 2*time.Second, func() bool { return restartCount.Load() == 1 })

	fc, _ := conf.LoadFile(configPath)
	if fc.ShellOn() {
		t.Error("shell should be persisted off")
	}
}

func TestBuiltinsToggleFirstRunDoesNotRestart(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	// Save a config with no AgentName — represents "configured shape on
	// disk but tunnel not paired yet". Restart would be pointless.
	if err := conf.SaveFile(configPath, &conf.FileConfig{}); err != nil {
		t.Fatal(err)
	}
	var restartCount atomic.Int32
	s := newTestServer(t, configPath, nil, &restartCount)
	// First-run gate is open; no cookie needed.
	off := false
	w := doJSON(s, http.MethodPost, "/api/config/builtins", map[string]any{"shell": off}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	// Give the scheduleRestart goroutine time to fire if it was going to.
	time.Sleep(400 * time.Millisecond)
	if restartCount.Load() != 0 {
		t.Errorf("restart was called on first-run save: %d times", restartCount.Load())
	}
}

// TestRegisterEndpoint hits POST /api/config/register against a fake
// portal, then asserts the merged config was saved (preserving locally
// managed Apps) and a restart was scheduled.
func TestRegisterEndpoint(t *testing.T) {
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"agent_name":"paired","server_addr":"edge","server_port":443,"protocol":"wss","token":"tok","remote_port":7100}`)
	}))
	defer portal.Close()

	configPath := filepath.Join(t.TempDir(), "agent.json")
	// Seed with a custom app — it must survive the pairing exchange.
	if err := conf.SaveFile(configPath, &conf.FileConfig{
		Apps: []conf.AppConfig{{Name: "before", Scheme: "http", Host: "127.0.0.1", Port: 9000, Enabled: true}},
	}); err != nil {
		t.Fatal(err)
	}
	var restartCount atomic.Int32
	s := newTestServer(t, configPath, nil, &restartCount)
	// configPath now exists; gate is closed. Mint a cookie to bypass.
	cookie, _ := s.sessions.Mint("tester")

	w := doJSON(s, http.MethodPost, "/api/config/register", map[string]any{
		"server": portal.URL, "code": "abc", "name": "host",
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	waitFor(t, 2*time.Second, func() bool { return restartCount.Load() == 1 })

	fc, _ := conf.LoadFile(configPath)
	if fc.AgentName != "paired" || fc.Token != "tok" || fc.RemotePort != 7100 {
		t.Errorf("portal fields not merged: %+v", fc)
	}
	if len(fc.Apps) != 1 || fc.Apps[0].Name != "before" {
		t.Errorf("locally-managed apps were clobbered by pairing: %+v", fc.Apps)
	}
}

// TestSessionExpiry — an expired cookie should be rejected. Sessions
// are stateless HMAC cookies now, so we expire by minting at an old
// virtual now() then validating at real now().
func TestSessionExpiry(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(configPath, &conf.FileConfig{AgentName: "x", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, configPath, nil, nil)
	// Pin time-of-mint to two hours ago and TTL is 1 minute (test default),
	// so the cookie is far past expiry by the time Validate runs.
	realNow := s.sessions.now
	s.sessions.now = func() time.Time { return realNow().Add(-2 * time.Hour) }
	cookie, _ := s.sessions.Mint("tester")
	s.sessions.now = realNow

	w := doJSON(s, http.MethodGet, "/api/config", nil, cookie)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expired cookie accepted: status %d body=%s", w.Code, w.Body.String())
	}
}

// helpers

func extractCookie(w *httptest.ResponseRecorder, name string) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func waitFor(t *testing.T, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
