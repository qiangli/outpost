package adminui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
)

// suggestionsTestSetup mints a logged-in session, plants a fake home
// directory carrying a ycode manifest, and returns (server, cookie) the
// caller drives the API with.
func suggestionsTestSetup(t *testing.T, manifest map[string]any, existingApps []conf.AppConfig) (*Server, string) {
	t.Helper()

	user, err := hostauth.CurrentUser()
	if err != nil || user == "" {
		t.Skip("cannot determine OS user")
	}

	fakeHome := t.TempDir()
	if manifest != nil {
		dir := filepath.Join(fakeHome, ".agents", "ycode")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir manifest dir: %v", err)
		}
		raw, _ := json.Marshal(manifest)
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o600); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}
	t.Setenv("HOME", fakeHome)

	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(cfgPath, &conf.FileConfig{
		AgentName: "x",
		Token:     "t",
		Apps:      existingApps,
	}); err != nil {
		t.Fatalf("save initial cfg: %v", err)
	}

	var restarts atomic.Int32
	s := newTestServer(t, cfgPath, map[string]string{user: "secret"}, &restarts)

	w := doJSON(s, http.MethodPost, "/api/login",
		map[string]string{"user": user, "password": "secret"}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookie := extractCookie(w, cookieName)
	if cookie == "" {
		t.Fatal("expected login cookie")
	}
	return s, cookie
}

// TestSuggestions_YcodeEmbeddedManifestSurfaces: an embedded-mode ycode
// gateway should appear as a one-click "add this app" suggestion.
func TestSuggestions_YcodeEmbeddedManifestSurfaces(t *testing.T) {
	manifest := map[string]any{
		"gateway": map[string]any{
			"podman": map[string]any{
				"socket": "/tmp/ycode-12345/podman.sock",
				"mode":   "embedded",
			},
		},
	}
	s, cookie := suggestionsTestSetup(t, manifest, nil)

	w := doJSON(s, http.MethodGet, "/api/apps/suggestions", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("suggestions: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Suggestions []Suggestion `json:"suggestions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var ycode *Suggestion
	for i, sg := range got.Suggestions {
		if sg.Name == "ycode-podman" {
			ycode = &got.Suggestions[i]
		}
	}
	if ycode == nil {
		t.Fatalf("expected ycode-podman suggestion, got: %+v", got.Suggestions)
	}
	if ycode.Source != "ycodeManifest" || ycode.Socket != "/tmp/ycode-12345/podman.sock" {
		t.Errorf("ycode-podman drift: %+v", *ycode)
	}
	if ycode.Role != "admin" {
		t.Errorf("ycode-podman role = %q, want admin", ycode.Role)
	}
}

// TestSuggestions_RemoteGatewaySkipped: a remote-mode ycode would loop
// back to cloudbox if re-exposed; outpost must not suggest it.
func TestSuggestions_RemoteGatewaySkipped(t *testing.T) {
	manifest := map[string]any{
		"gateway": map[string]any{
			"podman": map[string]any{
				"socket": "/tmp/ycode-12345/podman.sock",
				"mode":   "remote",
			},
		},
	}
	s, cookie := suggestionsTestSetup(t, manifest, nil)

	w := doJSON(s, http.MethodGet, "/api/apps/suggestions", nil, cookie)
	var got struct {
		Suggestions []Suggestion `json:"suggestions"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)

	for _, sg := range got.Suggestions {
		if strings.HasPrefix(sg.Name, "ycode-") && sg.Source == "ycodeManifest" {
			t.Errorf("remote-mode gateway should not be suggested: %+v", sg)
		}
	}
}

// TestSuggestions_FlagsExistingNames: a well-known suggestion whose name
// collides with an already-registered app should be flagged.
func TestSuggestions_FlagsExistingNames(t *testing.T) {
	// Use the always-present ycode manifest path so we have a stable
	// suggestion to compare existing-name behaviour against.
	manifest := map[string]any{
		"gateway": map[string]any{
			"podman": map[string]any{
				"socket": "/tmp/ycode-existing/podman.sock",
				"mode":   "embedded",
			},
		},
	}
	existing := []conf.AppConfig{
		{Name: "ycode-podman", Scheme: "unix", Socket: "/some/old/path", Enabled: true, Role: "admin"},
	}
	s, cookie := suggestionsTestSetup(t, manifest, existing)

	w := doJSON(s, http.MethodGet, "/api/apps/suggestions", nil, cookie)
	var got struct {
		Suggestions []Suggestion `json:"suggestions"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)

	for _, sg := range got.Suggestions {
		if sg.Name == "ycode-podman" && !sg.Existing {
			t.Errorf("ycode-podman should be flagged existing: %+v", sg)
		}
	}
}

// TestSuggestions_RequiresAuth: the endpoint sits inside the session
// gate; an unauthenticated caller must get 401.
func TestSuggestions_RequiresAuth(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	_ = conf.SaveFile(cfgPath, &conf.FileConfig{AgentName: "x", Token: "t"})
	var restarts atomic.Int32
	s := newTestServer(t, cfgPath, map[string]string{}, &restarts)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/apps/suggestions", nil)
	s.engine.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated suggestions = %d, want 401", w.Code)
	}
}
