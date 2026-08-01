package adminui

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// The REST surface for joining a PEER-hosted control plane. Mirrors the
// control-plane handlers on the hosting side; the load-bearing difference is
// that there is no reveal endpoint — a worker's copy of the credentials is
// never readable back out.
func TestClusterJoin_RoundTrip(t *testing.T) {
	const joinToken = "JOIN-TOKEN-a1b2c3"
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	s, cookie := loginAsCurrentUser(t, configPath, nil)

	// Fresh host: joins the cloudbox-hosted plane.
	w := doJSON(s, http.MethodGet, "/api/cluster/join", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET join: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Joined   bool   `json:"joined"`
		Endpoint string `json:"endpoint"`
		HasToken bool   `json:"has_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Joined || got.HasToken {
		t.Fatalf("fresh host reports a peer plane: %+v", got)
	}

	// Join.
	w = doJSON(s, http.MethodPost, "/api/cluster/join", map[string]any{
		"endpoint":    "10.0.0.5",
		"token":       joinToken,
		"stcp_secret": "s",
		"node_token":  "K10::node:n",
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("POST join: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), joinToken) {
		t.Errorf("POST /api/cluster/join echoed the token back: %s", w.Body.String())
	}
	fc, _ := conf.LoadFile(configPath)
	if fc.Cluster.JoinEndpoint != "10.0.0.5:7000" || fc.Cluster.JoinToken != joinToken {
		t.Fatalf("not persisted: %+v", fc.Cluster)
	}

	// Read back — presence only.
	w = doJSON(s, http.MethodGet, "/api/cluster/join", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET join after: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), joinToken) {
		t.Errorf("GET /api/cluster/join leaked the token: %s", w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Joined || !got.HasToken || got.Endpoint != "10.0.0.5:7000" {
		t.Errorf("view after join = %+v", got)
	}

	// GET /api/config must not carry it either — that is the response an
	// operator is most likely to have on screen.
	w = doJSON(s, http.MethodGet, "/api/config", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET config: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), joinToken) {
		t.Errorf("GET /api/config leaked the join token: %s", w.Body.String())
	}

	// Leave.
	w = doJSON(s, http.MethodDelete, "/api/cluster/join", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE join: %d %s", w.Code, w.Body.String())
	}
	fc, _ = conf.LoadFile(configPath)
	if fc.Cluster.JoinEndpoint != "" || fc.Cluster.JoinToken != "" ||
		fc.Cluster.NodeToken != "" || fc.Cluster.STCPSecret != "" {
		t.Errorf("DELETE left peer state behind: %+v", fc.Cluster)
	}
}

// Validation errors have to arrive as 400s, not 500s — the operator pasted
// something wrong, the daemon is fine.
func TestClusterJoin_ValidationIsBadRequest(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	s, cookie := loginAsCurrentUser(t, configPath, nil)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"URL instead of host:port", map[string]any{"endpoint": "https://10.0.0.5:7000", "token": "t"}},
		{"endpoint without token", map[string]any{"endpoint": "10.0.0.5:7000"}},
		{"port out of range", map[string]any{"endpoint": "10.0.0.5:70000", "token": "t"}},
		{"api_port out of range", map[string]any{"endpoint": "10.0.0.5", "token": "t", "api_port": 70000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doJSON(s, http.MethodPost, "/api/cluster/join", tc.body, cookie)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%s)", w.Code, w.Body.String())
			}
		})
	}
}
