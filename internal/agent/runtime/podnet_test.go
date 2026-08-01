package runtime

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestClassifyPodNetwork(t *testing.T) {
	tests := []struct {
		name        string
		podCIDR     string
		peerFlannel bool
		wantMode    PodNetworkMode
		wantCIDR    string
		wantOverlay bool
	}{
		{
			name:        "allocated cidr is the overlay",
			podCIDR:     "10.42.7.0/24",
			wantMode:    PodNetworkOverlay,
			wantCIDR:    "10.42.7.0/24",
			wantOverlay: true,
		},
		{
			name:        "whitespace is trimmed, still overlay",
			podCIDR:     "  10.42.9.0/24 ",
			wantMode:    PodNetworkOverlay,
			wantCIDR:    "10.42.9.0/24",
			wantOverlay: true,
		},
		{
			name:        "empty cidr is the single-node fallback",
			podCIDR:     "",
			wantMode:    PodNetworkSingleNodeFallback,
			wantCIDR:    FallbackPodCIDR,
			wantOverlay: false,
		},
		{
			name:        "blank cidr is the single-node fallback",
			podCIDR:     "   ",
			wantMode:    PodNetworkSingleNodeFallback,
			wantCIDR:    FallbackPodCIDR,
			wantOverlay: false,
		},
		{
			// The distinction the two-value enum could not make: an
			// empty CIDR on a peer plane is CORRECT, not a fallback.
			name:        "peer plane with no cidr is peer-flannel, not the fallback",
			podCIDR:     "",
			peerFlannel: true,
			wantMode:    PodNetworkPeerFlannel,
			wantCIDR:    "",
			wantOverlay: false,
		},
		{
			// A stale cloudbox carve must not resurrect a conflist on a
			// peer plane — the peer's controller-manager owns the CIDR.
			name:        "peer plane ignores a leftover cloudbox cidr",
			podCIDR:     "10.42.7.0/24",
			peerFlannel: true,
			wantMode:    PodNetworkPeerFlannel,
			wantCIDR:    "",
			wantOverlay: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyPodNetwork(tt.podCIDR, tt.peerFlannel)
			if got.Mode != tt.wantMode {
				t.Errorf("Mode = %q, want %q", got.Mode, tt.wantMode)
			}
			if got.PodCIDR != tt.wantCIDR {
				t.Errorf("PodCIDR = %q, want %q", got.PodCIDR, tt.wantCIDR)
			}
			if got.Overlay() != tt.wantOverlay {
				t.Errorf("Overlay() = %t, want %t", got.Overlay(), tt.wantOverlay)
			}
		})
	}
}

func TestOptionsPodNetwork(t *testing.T) {
	tests := []struct {
		name     string
		opts     Options
		wantMode PodNetworkMode
		wantCIDR string
	}{
		{
			name:     "overlay ignores the fallback override",
			opts:     Options{PodCIDR: "10.42.3.0/24", ExtraEnv: []string{"CNI_LOCAL_POD_CIDR=10.99.0.0/24"}},
			wantMode: PodNetworkOverlay,
			wantCIDR: "10.42.3.0/24",
		},
		{
			name:     "fallback reports the entrypoint default",
			opts:     Options{},
			wantMode: PodNetworkSingleNodeFallback,
			wantCIDR: FallbackPodCIDR,
		},
		{
			name:     "fallback honors an ExtraEnv override",
			opts:     Options{ExtraEnv: []string{"FOO=bar", "CNI_LOCAL_POD_CIDR=10.99.0.0/24"}},
			wantMode: PodNetworkSingleNodeFallback,
			wantCIDR: "10.99.0.0/24",
		},
		{
			name:     "blank override falls back to the default",
			opts:     Options{ExtraEnv: []string{"CNI_LOCAL_POD_CIDR="}},
			wantMode: PodNetworkSingleNodeFallback,
			wantCIDR: FallbackPodCIDR,
		},
		{
			name:     "peer flannel reports no cidr of its own",
			opts:     Options{PeerFlannel: true},
			wantMode: PodNetworkPeerFlannel,
			wantCIDR: "",
		},
		{
			// The fallback override belongs to a branch peer-flannel
			// never takes; honoring it would report a range no
			// container allocates from.
			name:     "peer flannel ignores the fallback override",
			opts:     Options{PeerFlannel: true, ExtraEnv: []string{"CNI_LOCAL_POD_CIDR=10.99.0.0/24"}},
			wantMode: PodNetworkPeerFlannel,
			wantCIDR: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.opts.PodNetwork()
			if got.Mode != tt.wantMode {
				t.Errorf("Mode = %q, want %q", got.Mode, tt.wantMode)
			}
			if got.PodCIDR != tt.wantCIDR {
				t.Errorf("PodCIDR = %q, want %q", got.PodCIDR, tt.wantCIDR)
			}
		})
	}
}

// The fallback must never be announced at Info — an operator scanning
// for warnings is exactly who needs to see it.
func TestPodNetworkLogLevels(t *testing.T) {
	tests := []struct {
		name        string
		podCIDR     string
		peerFlannel bool
		wantLevel   slog.Level
		wantSubs    []string
	}{
		{
			name:      "overlay logs at info with the cidr",
			podCIDR:   "10.42.5.0/24",
			wantLevel: slog.LevelInfo,
			wantSubs:  []string{"10.42.5.0/24", string(PodNetworkOverlay), "node-a"},
		},
		{
			name:      "fallback logs at warn",
			podCIDR:   "",
			wantLevel: slog.LevelWarn,
			wantSubs: []string{
				"NO pod network",
				"single-node",
				"multi-node",
				string(PodNetworkSingleNodeFallback),
				FallbackPodCIDR,
			},
		},
		{
			// Peer-flannel is a correct multi-node mode. Reusing the
			// fallback's WARN here would train operators to ignore the
			// one line that means real pod-IP collision.
			name:        "peer flannel logs at info, not the fallback warning",
			podCIDR:     "",
			peerFlannel: true,
			wantLevel:   slog.LevelInfo,
			wantSubs: []string{
				string(PodNetworkPeerFlannel),
				"tailscale0",
				"Node.spec.podCIDR",
				"node-a",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			ClassifyPodNetwork(tt.podCIDR, tt.peerFlannel).Log("node-a")

			out := buf.String()
			if !strings.Contains(out, "level="+tt.wantLevel.String()) {
				t.Errorf("expected level %s, got: %s", tt.wantLevel, out)
			}
			for _, sub := range tt.wantSubs {
				if !strings.Contains(out, sub) {
					t.Errorf("log missing %q, got: %s", sub, out)
				}
			}
		})
	}
}
