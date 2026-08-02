package admincore

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/vkcred"
)

func boolp(b bool) *bool { return &b }

// testVKBundleStr is a valid encoded vk bundle — the credential a peer join
// that selects virtual runtimes must now carry.
func testVKBundleStr(t *testing.T) string {
	t.Helper()
	enc, err := vkcred.Bundle{
		CA:         []byte("test-peer-ca"),
		Token:      "test-sa-token",
		Namespaces: []string{"default"},
	}.Encode()
	if err != nil {
		t.Fatalf("encode test bundle: %v", err)
	}
	return enc
}

// readCluster reads the persisted runtime selection back off disk, so these
// tests assert what a restart would actually load rather than what the return
// value happened to say.
func readCluster(t *testing.T, s *Server) conf.ClusterRuntimes {
	t.Helper()
	fc, err := conf.LoadFile(s.deps.ConfigPath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if fc.Cluster == nil {
		t.Fatal("no cluster block persisted")
	}
	return fc.Cluster.Runtimes
}

func joinFresh(t *testing.T, s *Server, p PeerPlaneParams) PeerPlaneResult {
	t.Helper()
	if p.Endpoint == nil {
		p.Endpoint = strp("10.0.0.5:7000")
	}
	if p.Token == nil {
		p.Token = strp("tunnel-token")
	}
	// Selecting a virtual runtime requires the vk credential; attach the test
	// bundle so runtime-selection tests exercise selection, not the gate (which
	// has its own tests in cluster_join_test.go).
	if len(p.Virtual) > 0 && p.VKBundle == nil {
		p.VKBundle = strp(testVKBundleStr(t))
	}
	res, err := s.JoinPeerPlane(p)
	if err != nil {
		t.Fatalf("JoinPeerPlane: %v", err)
	}
	return res
}

// THE REGRESSION GUARD. A join that does not ask for a runtime must keep
// producing exactly one agent node and no virtual node — the behaviour every
// existing peer worker was deployed under.
func TestJoinPeerPlane_DefaultStillAgentOnly(t *testing.T) {
	s := newTestServer(t)
	res := joinFresh(t, s, PeerPlaneParams{})

	if !res.RuntimeAgent || len(res.RuntimeVirtual) != 0 {
		t.Errorf("default join changed the selection: agent=%v virtual=%v",
			res.RuntimeAgent, res.RuntimeVirtual)
	}
	got := readCluster(t, s)
	if !got.Agent {
		t.Error("default join did not select the agent runtime")
	}
	if len(got.Virtual) != 0 {
		t.Errorf("default join selected virtual runtimes: %v", got.Virtual)
	}
}

// The point of the story: a peer worker can register vk nodes. vknode's
// kubeconfig loader already accepts the client certs k3s issues, so nothing
// but the selection was missing.
func TestJoinPeerPlane_ExplicitVirtualIsPersisted(t *testing.T) {
	tests := []struct {
		name        string
		params      PeerPlaneParams
		wantAgent   bool
		wantVirtual []string
	}{
		{
			name:        "virtual only replaces the implicit agent default",
			params:      PeerPlaneParams{Agent: boolp(false), Virtual: []string{"vk-podman"}},
			wantAgent:   false,
			wantVirtual: []string{"vk-podman"},
		},
		{
			name:        "virtual alongside the agent runtime",
			params:      PeerPlaneParams{Agent: boolp(true), Virtual: []string{"vk-podman", "vk-native"}},
			wantAgent:   true,
			wantVirtual: []string{"vk-podman", "vk-native"},
		},
		{
			name: "naming only virtual leaves the agent default off — the fallback " +
				"does not fire once the operator has selected something",
			params:      PeerPlaneParams{Virtual: []string{"vk-native"}},
			wantAgent:   false,
			wantVirtual: []string{"vk-native"},
		},
		{
			name:        "case and surrounding space are normalized",
			params:      PeerPlaneParams{Virtual: []string{" VK-Podman "}},
			wantAgent:   false,
			wantVirtual: []string{"vk-podman"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			res := joinFresh(t, s, tt.params)
			if res.RuntimeAgent != tt.wantAgent {
				t.Errorf("result agent = %v, want %v", res.RuntimeAgent, tt.wantAgent)
			}
			if !reflect.DeepEqual(res.RuntimeVirtual, tt.wantVirtual) {
				t.Errorf("result virtual = %v, want %v", res.RuntimeVirtual, tt.wantVirtual)
			}
			got := readCluster(t, s)
			if got.Agent != tt.wantAgent {
				t.Errorf("persisted agent = %v, want %v", got.Agent, tt.wantAgent)
			}
			if !reflect.DeepEqual(got.Virtual, tt.wantVirtual) {
				t.Errorf("persisted virtual = %v, want %v", got.Virtual, tt.wantVirtual)
			}
		})
	}
}

// A join must not silently start a runtime the operator did not ask for. This
// is the guard the pre-existing "only when no runtime was already selected"
// comment describes, now also covering the case where the prior selection came
// from an earlier join rather than from SetBuiltins.
func TestJoinPeerPlane_DoesNotClobberExistingSelection(t *testing.T) {
	t.Run("selection made by SetBuiltins survives a credential-only join", func(t *testing.T) {
		s := newTestServer(t)
		if _, err := s.SetBuiltins(BuiltinsParams{
			ClusterAgent:   boolp(false),
			ClusterVirtual: []string{"vk-podman"},
		}); err != nil {
			t.Fatalf("SetBuiltins: %v", err)
		}
		res := joinFresh(t, s, PeerPlaneParams{VKBundle: strp(testVKBundleStr(t))})
		if res.RuntimeAgent {
			t.Error("join started an agent runtime over a virtual-only selection")
		}
		got := readCluster(t, s)
		if got.Agent || !reflect.DeepEqual(got.Virtual, []string{"vk-podman"}) {
			t.Errorf("join clobbered the prior selection: %+v", got)
		}
	})

	t.Run("a rotated-token re-join leaves the selection alone", func(t *testing.T) {
		s := newTestServer(t)
		joinFresh(t, s, PeerPlaneParams{Virtual: []string{"vk-native", "vk-ollama"}})
		// Second call supplies only a rotated credential.
		if _, err := s.JoinPeerPlane(PeerPlaneParams{Token: strp("rotated")}); err != nil {
			t.Fatalf("re-join: %v", err)
		}
		got := readCluster(t, s)
		if got.Agent {
			t.Error("re-join added an agent runtime")
		}
		if !reflect.DeepEqual(got.Virtual, []string{"vk-native", "vk-ollama"}) {
			t.Errorf("re-join changed the virtual selection: %v", got.Virtual)
		}
	})
}

// Bad input must be refused at the join, not discovered at boot: a worker that
// persisted an unknown backend authenticates and then registers no node.
func TestJoinPeerPlane_RejectsBadRuntimeSelection(t *testing.T) {
	tests := []struct {
		name   string
		params PeerPlaneParams
	}{
		{"unknown backend", PeerPlaneParams{Virtual: []string{"vk-kubernetes"}}},
		{"the agent runtime is not a virtual backend", PeerPlaneParams{Virtual: []string{"agent"}}},
		{"duplicate backend", PeerPlaneParams{Virtual: []string{"vk-podman", "vk-podman"}}},
		{"deselecting everything explicitly", PeerPlaneParams{Agent: boolp(false), Virtual: []string{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			p := tt.params
			p.Endpoint = strp("10.0.0.5:7000")
			p.Token = strp("tunnel-token")
			if _, err := s.JoinPeerPlane(p); err == nil {
				t.Fatal("expected the join to be refused")
			}
		})
	}
}

// An agent.json written before runtimes existed must keep loading, and a join
// against it must still produce the agent-only default rather than an error.
func TestJoinPeerPlane_OldConfigWithoutRuntimesKey(t *testing.T) {
	s := newTestServer(t)
	legacy := []byte(`{"agent_name":"","cluster":{"enabled":true,"api_url":"https://127.0.0.1:6443"}}`)
	if err := os.WriteFile(s.deps.ConfigPath, legacy, 0o600); err != nil {
		t.Fatalf("seed legacy config: %v", err)
	}

	fc, err := conf.LoadFile(s.deps.ConfigPath)
	if err != nil {
		t.Fatalf("LoadFile on a config missing runtimes: %v", err)
	}
	if fc.Cluster.Runtimes.Agent || len(fc.Cluster.Runtimes.Virtual) != 0 {
		t.Fatalf("a missing runtimes key should decode to the zero selection, got %+v", fc.Cluster.Runtimes)
	}

	res := joinFresh(t, s, PeerPlaneParams{})
	if !res.RuntimeAgent || len(res.RuntimeVirtual) != 0 {
		t.Errorf("legacy config joined as %+v, want agent-only", res)
	}
	if got := readCluster(t, s); !got.Agent || len(got.Virtual) != 0 {
		t.Errorf("legacy config persisted as %+v, want agent-only", got)
	}

	// And the round trip preserves the other legacy fields.
	raw, err := os.ReadFile(s.deps.ConfigPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var back struct {
		Cluster struct {
			APIURL   string               `json:"api_url"`
			Runtimes conf.ClusterRuntimes `json:"runtimes"`
		} `json:"cluster"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Cluster.APIURL != "https://127.0.0.1:6443" {
		t.Errorf("join dropped api_url: %q", back.Cluster.APIURL)
	}
	if !back.Cluster.Runtimes.Agent {
		t.Error("join did not write the agent runtime into the legacy config")
	}
}

// The view reports the selection so an operator can tell a defaulted choice
// from one they made, without it ever carrying a credential.
func TestPeerPlaneView_ReportsRuntimes(t *testing.T) {
	s := newTestServer(t)
	joinFresh(t, s, PeerPlaneParams{Virtual: []string{"vk-podman"}})
	got, err := s.PeerPlaneView()
	if err != nil {
		t.Fatalf("PeerPlaneView: %v", err)
	}
	if got.RuntimeAgent || !reflect.DeepEqual(got.RuntimeVirtual, []string{"vk-podman"}) {
		t.Errorf("view = agent:%v virtual:%v", got.RuntimeAgent, got.RuntimeVirtual)
	}
	blob, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "tunnel-token") {
		t.Errorf("view leaked a credential: %s", blob)
	}
}
