package admincore

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/runtime"
)

// A peer-joined worker has no OverlayPodCIDR by construction — the peer's
// k3s controller-manager owns the allocation. Before the third mode
// existed that read as "single-node fallback", i.e. the status view told
// an operator their correctly-configured multi-node peer cluster was about
// to collide pod IPs.
func TestClusterViewPodNetworkMode(t *testing.T) {
	tests := []struct {
		name     string
		cluster  *conf.ClusterConfig
		wantMode string
		wantCIDR string
	}{
		{
			name:     "cloudbox overlay",
			cluster:  &conf.ClusterConfig{OverlayPodCIDR: "10.42.7.0/24"},
			wantMode: string(runtime.PodNetworkOverlay),
			wantCIDR: "10.42.7.0/24",
		},
		{
			name:     "no plane, no cidr, still the fallback",
			cluster:  &conf.ClusterConfig{},
			wantMode: string(runtime.PodNetworkSingleNodeFallback),
			wantCIDR: runtime.FallbackPodCIDR,
		},
		{
			name:     "peer plane reports peer-flannel with no cidr of its own",
			cluster:  &conf.ClusterConfig{JoinEndpoint: "peer.local:7000"},
			wantMode: string(runtime.PodNetworkPeerFlannel),
			wantCIDR: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := toClusterView(&conf.FileConfig{Cluster: tt.cluster})
			if v.PodNetworkMode != tt.wantMode {
				t.Errorf("PodNetworkMode = %q, want %q", v.PodNetworkMode, tt.wantMode)
			}
			if v.PodCIDR != tt.wantCIDR {
				t.Errorf("PodCIDR = %q, want %q", v.PodCIDR, tt.wantCIDR)
			}
		})
	}
}

// The overlay auth key is a standing tailnet credential. The cluster view
// is the surface the SPA, `outpost status` and the MCP resource all
// render, so it is where a leak would be widest.
func TestClusterViewNeverRendersOverlayAuthKey(t *testing.T) {
	const key = "tskey-auth-LEAKCANARY-doNotRender"
	v := toClusterView(&conf.FileConfig{Cluster: &conf.ClusterConfig{
		JoinEndpoint:       "peer.local:7000",
		OverlayLoginServer: "https://hs.example",
		OverlayAuthKey:     key,
		JoinToken:          "join-secret",
		NodeToken:          "K10node-secret",
		STCPSecret:         "stcp-secret",
	}})
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal cluster view: %v", err)
	}
	for _, secret := range []string{key, "join-secret", "K10node-secret", "stcp-secret"} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("cluster view rendered a credential (%q): %s", secret, blob)
		}
	}
}
