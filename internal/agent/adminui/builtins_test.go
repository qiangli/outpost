package adminui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
)

// TestBuiltinsTogglePodmanOllama — toggling podman/ollama via /api/config/builtins
// persists to the FileConfig and is reflected in /api/config.
func TestBuiltinsTogglePodmanOllama(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	// Seed a configured (paired) FileConfig so requireSession bypass is
	// off and we exercise the cookie path. Login as the OS user.
	if err := conf.SaveFile(configPath, &conf.FileConfig{AgentName: "h"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	user, err := hostauth.CurrentUser()
	if err != nil || user == "" {
		t.Skip("cannot determine OS user")
	}
	s := newTestServer(t, configPath, map[string]string{user: "secret"}, nil)
	w := doJSON(s, http.MethodPost, "/api/login", map[string]string{"user": user, "password": "secret"}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookie := extractCookie(w, cookieName)
	if cookie == "" {
		t.Fatal("missing session cookie after login")
	}

	w = doJSON(s, http.MethodPost, "/api/config/builtins",
		map[string]any{"podman": true, "ollama": true}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("toggle podman/ollama on: %d %s", w.Code, w.Body.String())
	}

	fc, err := conf.LoadFile(configPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !fc.PodmanOn() || !fc.OllamaOn() {
		t.Fatalf("flags not persisted: PodmanOn=%v OllamaOn=%v", fc.PodmanOn(), fc.OllamaOn())
	}

	// /api/config should reflect podman + ollama as enabled. Available may
	// be true or false depending on the test box; we only assert Enabled
	// here.
	w = doJSON(s, http.MethodGet, "/api/config", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("get config: %d %s", w.Code, w.Body.String())
	}
	var view struct {
		Podman builtinView `json:"podman"`
		Ollama builtinView `json:"ollama"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if !view.Podman.Enabled {
		t.Fatalf("podman.enabled = false, want true")
	}
	if !view.Ollama.Enabled {
		t.Fatalf("ollama.enabled = false, want true")
	}
}

// TestUpsertAppAcceptsURL — POST /api/apps with a single "url" field
// splits cleanly into scheme/host/port (or scheme/socket) on the server.
func TestUpsertAppAcceptsURL(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")

	s := newTestServer(t, configPath, nil, nil)
	// First-run bypass is on (loopback + no config yet), so we can POST
	// without a cookie.

	cases := []struct {
		name     string
		body     map[string]any
		wantSchm string
		wantHost string
		wantPort int
		wantSock string
	}{
		{
			name:     "http with port",
			body:     map[string]any{"name": "weba", "url": "http://localhost:8080", "role": "user", "enabled": true},
			wantSchm: "http", wantHost: "localhost", wantPort: 8080,
		},
		{
			name:     "unix socket",
			body:     map[string]any{"name": "podsock", "url": "unix:///tmp/foo.sock", "role": "admin", "enabled": true},
			wantSchm: "unix", wantSock: "/tmp/foo.sock",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doJSON(s, http.MethodPost, "/api/apps", tc.body, "")
			if w.Code != http.StatusOK {
				t.Fatalf("upsert: %d %s", w.Code, w.Body.String())
			}
			fc, err := conf.LoadFile(configPath)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			var found *conf.AppConfig
			for i := range fc.Apps {
				if fc.Apps[i].Name == tc.body["name"] {
					found = &fc.Apps[i]
				}
			}
			if found == nil {
				t.Fatalf("app %q not persisted; apps=%+v", tc.body["name"], fc.Apps)
			}
			if found.Scheme != tc.wantSchm || found.Host != tc.wantHost ||
				found.Port != tc.wantPort || found.Socket != tc.wantSock {
				t.Fatalf("got (scheme=%q host=%q port=%d socket=%q); want (scheme=%q host=%q port=%d socket=%q)",
					found.Scheme, found.Host, found.Port, found.Socket,
					tc.wantSchm, tc.wantHost, tc.wantPort, tc.wantSock)
			}
		})
	}
}

// TestUpsertAppRejectsReservedName — admin UI reserves a few top-level
// paths for its own routes; allowing an app with one of those names
// would shadow the admin API or local-proxy.
func TestUpsertAppRejectsReservedName(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	s := newTestServer(t, configPath, nil, nil)
	for _, name := range []string{"api", "static", "healthz", "index.html", "app", "API"} {
		w := doJSON(s, http.MethodPost, "/api/apps",
			map[string]any{"name": name, "url": "http://localhost:8080", "role": "user", "enabled": true}, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("name %q: expected 400, got %d (%s)", name, w.Code, w.Body.String())
		}
	}
}

// TestLocalAppProxy — registered apps are reachable on the admin UI
// listener at `/<name>/...`, gated by the admin session. Drives the gin
// engine through httptest.NewServer because net/http/httputil's reverse
// proxy needs a real ResponseWriter (httptest.ResponseRecorder doesn't
// implement http.CloseNotifier).
func TestLocalAppProxy(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	if err := conf.SaveFile(configPath, &conf.FileConfig{AgentName: "h"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	gotPath := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(upstream.Close)

	user, err := hostauth.CurrentUser()
	if err != nil || user == "" {
		t.Skip("cannot determine OS user")
	}
	s := newTestServer(t, configPath, map[string]string{user: "secret"}, nil)
	if err := s.deps.Apps.RegisterWithRole("fake", upstream.URL, "user"); err != nil {
		t.Fatalf("register: %v", err)
	}

	adminTest := httptest.NewServer(s.engine)
	t.Cleanup(adminTest.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// Without a session cookie: 401.
	resp, err := client.Get(adminTest.URL + "/fake/hello/world")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /fake/...: got %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Log in.
	loginBody, _ := json.Marshal(map[string]string{"user": user, "password": "secret"})
	resp, err = client.Post(adminTest.URL+"/api/login", "application/json", bytes.NewReader(loginBody))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login status %d body=%s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	// Authenticated proxy call.
	resp, err = client.Get(adminTest.URL + "/fake/hello/world?q=1")
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated /fake/...: got %d body=%s", resp.StatusCode, body)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q, want hello", body)
	}
	select {
	case p := <-gotPath:
		if p != "/hello/world" {
			t.Fatalf("upstream saw path %q, want /hello/world", p)
		}
	default:
		t.Fatal("upstream was not hit")
	}

	// Unknown app: 404.
	resp, err = client.Get(adminTest.URL + "/nosuch/")
	if err != nil {
		t.Fatalf("get nosuch: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown app: got %d, want 404", resp.StatusCode)
	}
}

// TestUpsertAppBadURL — invalid URL forms produce a 400 with a readable error.
func TestUpsertAppBadURL(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	s := newTestServer(t, configPath, nil, nil)
	w := doJSON(s, http.MethodPost, "/api/apps",
		map[string]any{"name": "x", "url": "ftp://nope:21", "role": "user", "enabled": true}, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for ftp url, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestLoopbackBypassOnlyOnLoopback — when loopbackOnly is false (LAN bind),
// even an unconfigured outpost must require a session cookie.
func TestLoopbackBypassOnlyOnLoopback(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	s := newTestServer(t, configPath, nil, nil)
	s.loopbackOnly = false // simulate LAN bind

	w := doJSON(s, http.MethodGet, "/api/config", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("LAN-bound unconfigured outpost: got %d, want 401", w.Code)
	}
}

// TestSessionSurvivesRestart — the user's "switch on/off builtin logouts"
// bug. A cookie minted by one Server instance must still validate on a
// freshly-constructed instance, as long as both share the same persisted
// SessionKey. Otherwise toggling a built-in (which re-execs the binary)
// silently logs the admin out.
func TestSessionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	if err := conf.SaveFile(configPath, &conf.FileConfig{AgentName: "h"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// EnsureAdminSessionKey both generates and persists the key.
	fc, _ := conf.LoadFile(configPath)
	key, err := conf.EnsureAdminSessionKey(configPath, fc)
	if err != nil {
		t.Fatalf("ensure key: %v", err)
	}

	// Server A mints a cookie.
	srvA := &sessionStore{secret: key, ttl: time.Hour, now: time.Now, revoked: map[string]time.Time{}}
	cookie, err := srvA.Mint("admin")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// Reload the FileConfig as a fresh process would, pull the persisted
	// key, build a new sessionStore, and validate the prior cookie.
	fc2, _ := conf.LoadFile(configPath)
	key2, err := conf.EnsureAdminSessionKey(configPath, fc2)
	if err != nil {
		t.Fatalf("re-ensure key: %v", err)
	}
	srvB := &sessionStore{secret: key2, ttl: time.Hour, now: time.Now, revoked: map[string]time.Time{}}
	user, ok := srvB.Validate(cookie)
	if !ok {
		t.Fatalf("cookie minted by srvA was rejected by srvB after restart")
	}
	if user != "admin" {
		t.Fatalf("validated user %q, want admin", user)
	}
}

// TestScheduleRestartDebounces — rapid back-to-back toggle saves (now
// fired one-per-switch by the new auto-saving UI) must collapse into a
// single Restart() call, not four.
func TestScheduleRestartDebounces(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	if err := conf.SaveFile(configPath, &conf.FileConfig{AgentName: "h"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	user, err := hostauth.CurrentUser()
	if err != nil || user == "" {
		t.Skip("cannot determine OS user")
	}
	var restarts atomic.Int32
	s := newTestServer(t, configPath, map[string]string{user: "secret"}, &restarts)
	w := doJSON(s, http.MethodPost, "/api/login",
		map[string]string{"user": user, "password": "secret"}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookie := extractCookie(w, cookieName)

	// Fire four toggles in quick succession.
	for _, key := range []string{"shell", "desktop", "clipboard", "ssh"} {
		w := doJSON(s, http.MethodPost, "/api/config/builtins",
			map[string]any{key: false}, cookie)
		if w.Code != http.StatusOK {
			t.Fatalf("toggle %s: %d %s", key, w.Code, w.Body.String())
		}
	}
	// Wait long enough for the debounce window to expire.
	time.Sleep(1500 * time.Millisecond)
	got := restarts.Load()
	if got != 1 {
		t.Fatalf("expected 1 restart for 4 rapid saves, got %d", got)
	}
}
