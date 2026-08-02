package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/runtime"
)

// applyPeerOverlayRelay is the fail-closed half of the tailnet
// registration path (docs/adr-peer-dks-pod-network.md — the hop the ADR
// left unresolved): without cloudbox's retained STCP secret the runtime
// container has no route to Headscale, flannel gets an addressless
// tailscale0, and the entrypoint's IPv4 gate kills the container after two
// minutes of nothing. Failing here instead says WHY, before a single-use
// auth key is burned or a container started.
func TestApplyPeerOverlayRelayFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		fc      *conf.FileConfig
		wantErr string
	}{
		{
			name: "missing relay secret",
			fc: &conf.FileConfig{
				ServerAddr: "ai.example.io", ServerPort: 443,
				Cluster: &conf.ClusterConfig{JoinEndpoint: "10.0.0.5:7000"},
			},
			wantErr: "cloud_stcp_secret",
		},
		{
			name: "missing cloudbox pairing",
			fc: &conf.FileConfig{
				Cluster: &conf.ClusterConfig{
					JoinEndpoint: "10.0.0.5:7000", CloudSTCPSecret: "cloud-secret",
				},
			},
			wantErr: "server_addr",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts runtime.Options
			err := applyPeerOverlayRelay(tt.fc, &opts)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not name the missing piece %q", err, tt.wantErr)
			}
			if opts.OverlayRelayActive() {
				t.Error("a failed relay setup left the relay active")
			}
		})
	}
}

// The success path: the relay is the cloudbox PAIRING (which a peer join
// leaves intact) plus the retained relay secret, aimed at cloudbox's own
// publisher user — never the peer's, whose frps has no overlay-control.
func TestApplyPeerOverlayRelayUsesCloudboxPairing(t *testing.T) {
	fc := &conf.FileConfig{
		ServerAddr: "ai.example.io",
		ServerPort: 443,
		Protocol:   "wss",
		Token:      "matrix-token",
		Cluster: &conf.ClusterConfig{
			JoinEndpoint:    "10.0.0.5:7000",
			JoinToken:       "peer-tunnel-token",
			STCPSecret:      "peer-secret",
			CloudSTCPSecret: "cloud-secret",
		},
	}
	var opts runtime.Options
	if err := applyPeerOverlayRelay(fc, &opts); err != nil {
		t.Fatalf("applyPeerOverlayRelay: %v", err)
	}
	if opts.OverlayRelayHost != "ai.example.io" || opts.OverlayRelayPort != 443 {
		t.Errorf("relay endpoint = %s:%d, want the cloudbox pairing", opts.OverlayRelayHost, opts.OverlayRelayPort)
	}
	if opts.OverlayRelayProtocol != "wss" || opts.OverlayRelayToken != "matrix-token" {
		t.Errorf("relay transport = %q/%q, want the pairing's", opts.OverlayRelayProtocol, opts.OverlayRelayToken)
	}
	if opts.OverlayRelaySecret != "cloud-secret" {
		t.Errorf("relay secret = %q, want cloudbox's — NOT the peer's %q", opts.OverlayRelaySecret, fc.Cluster.STCPSecret)
	}
	if opts.OverlayRelayUser != conf.CloudboxPublisherUser {
		t.Errorf("relay user = %q, want %q", opts.OverlayRelayUser, conf.CloudboxPublisherUser)
	}
	if !opts.OverlayRelayActive() {
		t.Error("a fully-populated relay reports inactive")
	}
}

// The agent-runtime supervisor must honor cancellation promptly on every
// exit path — a daemon restart cannot hang on a backoff timer or keep
// re-upping a container after shutdown began.
func TestSuperviseAgentRuntimeStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		superviseAgentRuntime(ctx, runtime.Options{
			AgentName: "n", NodeToken: "t",
			PodmanBin: "outpost-test-no-such-binary",
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("superviseAgentRuntime did not return after ctx cancel")
	}
}
