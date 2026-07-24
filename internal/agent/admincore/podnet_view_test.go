package admincore

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/runtime"
)

// The pod-network mode is derived read-only state, but it has to reach
// every read surface — `outpost status --json` and the MCP status
// resource both render SafeView.Cluster.
func TestClusterView_PodNetworkMode(t *testing.T) {
	on := true
	tests := []struct {
		name     string
		podCIDR  string
		wantMode string
		wantCIDR string
	}{
		{
			name:     "allocated cidr reports overlay",
			podCIDR:  "10.42.11.0/24",
			wantMode: string(runtime.PodNetworkOverlay),
			wantCIDR: "10.42.11.0/24",
		},
		{
			name:     "missing cidr reports the single-node fallback",
			podCIDR:  "",
			wantMode: string(runtime.PodNetworkSingleNodeFallback),
			wantCIDR: runtime.FallbackPodCIDR,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgPath := filepath.Join(t.TempDir(), "agent.json")
			fc := &conf.FileConfig{
				AgentName: "host1",
				Token:     "t",
				Cluster: &conf.ClusterConfig{
					Enabled:        &on,
					Mode:           "agent",
					OverlayPodCIDR: tt.podCIDR,
				},
			}
			if err := conf.SaveFile(cfgPath, fc); err != nil {
				t.Fatal(err)
			}
			core, err := New(Deps{ConfigPath: cfgPath, Apps: agent.NewAppRegistry()})
			if err != nil {
				t.Fatal(err)
			}
			view, err := core.SafeView()
			if err != nil {
				t.Fatalf("SafeView: %v", err)
			}
			if view.Cluster.PodNetworkMode != tt.wantMode {
				t.Errorf("PodNetworkMode = %q, want %q", view.Cluster.PodNetworkMode, tt.wantMode)
			}
			if view.Cluster.PodCIDR != tt.wantCIDR {
				t.Errorf("PodCIDR = %q, want %q", view.Cluster.PodCIDR, tt.wantCIDR)
			}

			// The JSON keys are the operator-visible contract.
			blob, err := json.Marshal(view.Cluster)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(blob, &wire); err != nil {
				t.Fatal(err)
			}
			if wire["pod_network_mode"] != tt.wantMode {
				t.Errorf("wire pod_network_mode = %v, want %q", wire["pod_network_mode"], tt.wantMode)
			}
			if wire["pod_cidr"] != tt.wantCIDR {
				t.Errorf("wire pod_cidr = %v, want %q", wire["pod_cidr"], tt.wantCIDR)
			}
		})
	}
}

// An outpost with no cluster block at all reports nothing — the field
// is omitempty so unconfigured hosts don't grow a misleading
// "single-node-fallback" badge.
func TestClusterView_UnconfiguredOmitsPodNetwork(t *testing.T) {
	core, _ := newTestCore(t)
	view, err := core.SafeView()
	if err != nil {
		t.Fatalf("SafeView: %v", err)
	}
	if view.Cluster.PodNetworkMode != "" || view.Cluster.PodCIDR != "" {
		t.Errorf("expected empty pod-network fields, got %+v", view.Cluster)
	}
}
