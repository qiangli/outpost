package admincore

import (
	"context"
	"errors"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/runtime"
)

// stubNodeToken swaps the podman-exec seam for the duration of a test. No unit
// test may reach a container engine.
func stubNodeToken(t *testing.T, fn func(context.Context, runtime.ServerOptions) (string, error)) {
	t.Helper()
	prev := serverNodeToken
	serverNodeToken = fn
	t.Cleanup(func() { serverNodeToken = prev })
}

func TestControlPlaneNodeToken(t *testing.T) {
	on := true
	tests := []struct {
		name       string
		fc         *conf.FileConfig
		fetch      func(context.Context, runtime.ServerOptions) (string, error)
		wantToken  string
		wantStatus int // 0 = success
		wantNode   string
	}{
		{
			name: "hosting host returns the token and the endpoint to pair it with",
			fc: &conf.FileConfig{
				AgentName: "host1",
				Cluster: &conf.ClusterConfig{
					ControlPlane: &on, TunnelBindAddr: "10.0.0.5", TunnelBindPort: 7100,
				},
			},
			fetch:     func(context.Context, runtime.ServerOptions) (string, error) { return "K10abc::node:xyz\n", nil },
			wantToken: "K10abc::node:xyz\n",
			wantNode:  "host1",
		},
		{
			name: "the node-name override picks the container",
			fc: &conf.FileConfig{
				AgentName: "host1",
				Cluster:   &conf.ClusterConfig{ControlPlane: &on, NodeName: "plane"},
			},
			fetch:     func(context.Context, runtime.ServerOptions) (string, error) { return "K10", nil },
			wantToken: "K10",
			wantNode:  "plane",
		},
		{
			// A worker asking for the node token is asking the wrong machine.
			// Returning an empty value would read as "the plane has no token".
			name:       "a host that hosts nothing is refused",
			fc:         &conf.FileConfig{AgentName: "host1"},
			fetch:      func(context.Context, runtime.ServerOptions) (string, error) { return "K10", nil },
			wantStatus: 400,
		},
		{
			name: "a container that isn't up yet is unavailable, not a config error",
			fc: &conf.FileConfig{
				AgentName: "host1",
				Cluster:   &conf.ClusterConfig{ControlPlane: &on},
			},
			fetch: func(context.Context, runtime.ServerOptions) (string, error) {
				return "", errors.New("no such container")
			},
			wantStatus: 503,
		},
		{
			name:       "no node name is a clear error rather than an empty container name",
			fc:         &conf.FileConfig{Cluster: &conf.ClusterConfig{ControlPlane: &on}},
			fetch:      func(context.Context, runtime.ServerOptions) (string, error) { return "K10", nil },
			wantStatus: 400,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			if err := conf.SaveFile(s.deps.ConfigPath, tt.fc); err != nil {
				t.Fatal(err)
			}
			var gotNode string
			stubNodeToken(t, func(ctx context.Context, opts runtime.ServerOptions) (string, error) {
				gotNode = opts.AgentName
				return tt.fetch(ctx, opts)
			})

			got, err := s.ControlPlaneNodeToken(context.Background())
			if tt.wantStatus != 0 {
				if err == nil {
					t.Fatalf("wanted status %d, got token %q", tt.wantStatus, got.NodeToken)
				}
				ae := AsAPIError(err)
				if ae == nil || ae.Status != tt.wantStatus {
					t.Fatalf("err = %v, want status %d", err, tt.wantStatus)
				}
				if got.NodeToken != "" {
					t.Errorf("failed call still returned a token: %q", got.NodeToken)
				}
				return
			}
			if err != nil {
				t.Fatalf("ControlPlaneNodeToken: %v", err)
			}
			if got.NodeToken != tt.wantToken {
				t.Errorf("token = %q, want %q", got.NodeToken, tt.wantToken)
			}
			if gotNode != tt.wantNode {
				t.Errorf("read from node %q, want %q", gotNode, tt.wantNode)
			}
		})
	}
}

// The endpoint travels with the token because a worker needs both, and the
// bind defaults must survive the trip.
func TestControlPlaneNodeToken_EndpointDefaults(t *testing.T) {
	on := true
	s := newTestServer(t)
	if err := conf.SaveFile(s.deps.ConfigPath, &conf.FileConfig{
		AgentName: "host1",
		Cluster:   &conf.ClusterConfig{ControlPlane: &on},
	}); err != nil {
		t.Fatal(err)
	}
	stubNodeToken(t, func(context.Context, runtime.ServerOptions) (string, error) { return "K10", nil })

	got, err := s.ControlPlaneNodeToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint != "127.0.0.1:7000" {
		t.Errorf("endpoint = %q, want the loopback default", got.Endpoint)
	}
}
