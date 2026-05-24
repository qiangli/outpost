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

// goldenKubeconfig is what k3s writes to /etc/rancher/k3s/k3s.yaml —
// inline CA + inline token, single context. Synthetic values; we only
// care that the parser pulls out the three fields we persist.
const goldenKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: ZmFrZS1jYQ==
    server: https://127.0.0.1:6443
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
users:
- name: default
  user:
    token: fake-bearer-token
`

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

func TestCluster_PasteKubeconfigPersistsThreeFields(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	s, cookie := loginAsCurrentUser(t, configPath, nil)

	w := doJSON(s, http.MethodPost, "/api/cluster/kubeconfig", map[string]any{
		"kubeconfig": goldenKubeconfig,
		"enable":     true,
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("paste kubeconfig: %d %s", w.Code, w.Body.String())
	}

	fc, err := conf.LoadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Cluster == nil {
		t.Fatal("Cluster not persisted")
	}
	if fc.Cluster.APIURL != "https://127.0.0.1:6443" {
		t.Errorf("APIURL: %q", fc.Cluster.APIURL)
	}
	if fc.Cluster.Token != "fake-bearer-token" {
		t.Errorf("Token: %q", fc.Cluster.Token)
	}
	if string(fc.Cluster.CA) != "fake-ca" {
		t.Errorf("CA: %q (decoded)", string(fc.Cluster.CA))
	}
	if !fc.Cluster.Enabled {
		t.Error("Enabled flag not set despite enable=true")
	}
}

func TestCluster_PasteKubeconfig_NodeNameOverride(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	s, cookie := loginAsCurrentUser(t, configPath, nil)

	w := doJSON(s, http.MethodPost, "/api/cluster/kubeconfig", map[string]any{
		"kubeconfig": goldenKubeconfig,
		"node_name":  "cottage-vk",
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("paste: %d %s", w.Code, w.Body.String())
	}

	fc, _ := conf.LoadFile(configPath)
	if fc.Cluster.NodeName != "cottage-vk" {
		t.Errorf("NodeName override not persisted: %q", fc.Cluster.NodeName)
	}
	// ClusterNodeName() resolves the override.
	if fc.ClusterNodeName() != "cottage-vk" {
		t.Errorf("ClusterNodeName(): %q", fc.ClusterNodeName())
	}
}

func TestCluster_PasteKubeconfig_InvalidYAMLReturns400(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	s, cookie := loginAsCurrentUser(t, configPath, nil)

	w := doJSON(s, http.MethodPost, "/api/cluster/kubeconfig", map[string]any{
		"kubeconfig": "not a real kubeconfig",
	}, cookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid yaml: got %d want 400, body=%s", w.Code, w.Body.String())
	}

	fc, _ := conf.LoadFile(configPath)
	if fc != nil && fc.Cluster != nil {
		t.Errorf("Cluster shouldn't have been written for invalid input: %+v", fc.Cluster)
	}
}

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
			Enabled: true,
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
			Enabled: true,
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
