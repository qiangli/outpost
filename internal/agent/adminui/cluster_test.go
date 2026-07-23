package adminui

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
)

// loginAsCurrentUser handles the common test bootstrap: pair the
// outpost, log the running OS user in, and return the session cookie.
// Skips when the OS user can't be resolved (CI without a real user).
func loginAsCurrentUser(t *testing.T, configPath string, seed *conf.FileConfig) (*Server, string) {
	t.Helper()
	if seed == nil {
		seed = &conf.FileConfig{AgentName: "h"}
	}
	if err := conf.SaveFile(configPath, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	user, err := hostauth.CurrentUser()
	if err != nil || user == "" {
		t.Skip("cannot determine OS user")
	}
	s := newTestServer(t, configPath, map[string]string{user: "secret"}, nil)
	w := doJSON(s, http.MethodPost, "/api/login",
		map[string]string{"user": user, "password": "secret"}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookie := extractCookie(w, cookieName)
	if cookie == "" {
		t.Fatal("missing session cookie after login")
	}
	return s, cookie
}

// POST /api/cluster/kubeconfig (paste path) was removed — outposts
// only join their owning cloudbox's cluster now. The three tests
// that previously covered the paste flow (persist three fields,
// node-name override, invalid YAML → 400) are gone with the
// endpoint. TestCluster_ToggleViaBuiltins below still exercises
// flipping the cluster enable flag through /api/config/builtins.

func TestCluster_ToggleViaBuiltins(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	// Pre-seed with credentials already pasted so toggling On is meaningful.
	seed := &conf.FileConfig{
		AgentName: "h",
		Cluster: &conf.ClusterConfig{
			APIURL: "https://127.0.0.1:6443",
			Token:  "t",
		},
	}
	s, cookie := loginAsCurrentUser(t, configPath, seed)

	w := doJSON(s, http.MethodPost, "/api/config/builtins",
		map[string]any{"cluster": true}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("toggle on: %d %s", w.Code, w.Body.String())
	}
	fc, _ := conf.LoadFile(configPath)
	if !fc.ClusterOn() {
		t.Fatalf("toggle did not flip ClusterEnabled: %+v", fc.Cluster)
	}

	w = doJSON(s, http.MethodPost, "/api/config/builtins",
		map[string]any{"cluster": false}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("toggle off: %d %s", w.Code, w.Body.String())
	}
	fc, _ = conf.LoadFile(configPath)
	if fc.ClusterOn() {
		t.Fatalf("toggle off did not flip ClusterEnabled: %+v", fc.Cluster)
	}
	// Credentials must survive the off toggle — operator can flip back
	// on without re-pasting.
	if fc.Cluster == nil || fc.Cluster.APIURL == "" || fc.Cluster.Token == "" {
		t.Errorf("credentials lost on toggle off: %+v", fc.Cluster)
	}
}

func TestCluster_ConfigViewRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	seed := &conf.FileConfig{
		AgentName: "h",
		Cluster: &conf.ClusterConfig{
			Enabled: ptrTrue(),
			APIURL:  "https://k.example",
			Token:   "should-never-leave-the-agent",
			CA:      []byte("ca-bytes"),
		},
	}
	s, cookie := loginAsCurrentUser(t, configPath, seed)

	w := doJSON(s, http.MethodGet, "/api/config", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("get config: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "should-never-leave-the-agent") {
		t.Errorf("Token leaked into /api/config response: %s", body)
	}
	if strings.Contains(body, "ca-bytes") {
		t.Errorf("CA leaked into /api/config response: %s", body)
	}
	var view struct {
		Cluster clusterView `json:"cluster"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	if !view.Cluster.Enabled || view.Cluster.APIURL != "https://k.example" {
		t.Errorf("cluster view: %+v", view.Cluster)
	}
	if !view.Cluster.HasToken || !view.Cluster.HasCA {
		t.Errorf("has_token/has_ca: %+v", view.Cluster)
	}
	// NodeName defaults to AgentName when not overridden.
	if view.Cluster.NodeName != "h" {
		t.Errorf("NodeName default: %q want %q", view.Cluster.NodeName, "h")
	}
}

func TestCluster_ClearKubeconfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	seed := &conf.FileConfig{
		AgentName: "h",
		Cluster: &conf.ClusterConfig{
			Enabled: ptrTrue(),
			APIURL:  "https://k",
			Token:   "t",
		},
	}
	s, cookie := loginAsCurrentUser(t, configPath, seed)

	w := doJSON(s, http.MethodDelete, "/api/cluster/kubeconfig", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	fc, _ := conf.LoadFile(configPath)
	if fc.Cluster != nil {
		t.Errorf("Cluster not cleared: %+v", fc.Cluster)
	}
}

// ptrTrue returns a *bool set to true — ClusterConfig.Enabled is a *bool
// (default-on nil-semantics); tests that want an explicit opt-in use this.
func ptrTrue() *bool { b := true; return &b }
