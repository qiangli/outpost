package main

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func waitDialable(addr string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func boolPtr(b bool) *bool { return &b }

// The wiring this test exists for: a host marked as the control plane must
// actually be LISTENING after boot. Everything else in the peer-hosted-plane
// story was already built and tested in isolation; the missing piece was that
// nothing started the server, so a worker's frpc got "connection refused"
// with nothing visibly wrong on either side.
func TestStartControlPlaneTunnel_ListensWhenControlPlane(t *testing.T) {
	port := freePort(t)
	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	fc := &conf.FileConfig{
		AgentName: "cp-host",
		Cluster: &conf.ClusterConfig{
			ControlPlane:   boolPtr(true),
			TunnelBindAddr: "127.0.0.1",
			TunnelBindPort: port,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)

	startControlPlaneTunnel(gctx, g, fc, cfgPath)

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if !waitDialable(addr, 5*time.Second) {
		t.Fatalf("control-plane tunnel never accepted a connection on %s", addr)
	}

	// The token must have been minted and persisted as a side effect —
	// a server that starts but whose token was not saved would reject every
	// worker after the next restart.
	if len(fc.Cluster.TunnelToken) < 32 {
		t.Errorf("tunnel token not minted: %q", fc.Cluster.TunnelToken)
	}
	reloaded, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.Cluster == nil || reloaded.Cluster.TunnelToken != fc.Cluster.TunnelToken {
		t.Error("tunnel token was not persisted at boot")
	}

	// THE REGRESSION THIS TEST EXISTS FOR, second half. The daemon
	// self-restarts by cancelling this errgroup, waiting for it, and
	// re-execing. frp's Run never returns — not on ctx cancel, not after
	// Close — so wiring it into the group makes Wait() block forever and
	// hangs every pairing change, builtin toggle and upgrade. The daemon
	// would look alive while silently applying nothing.
	cancel()
	waited := make(chan error, 1)
	go func() { waited <- g.Wait() }()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("errgroup did not drain after cancel — a self-restart would hang here")
	}

	// Close must free the bind port, or the re-exec cannot rebind it.
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("bind port still held after shutdown: %v", err)
	}
	_ = l.Close()
}

// Nearly every host is not the control plane. Starting a tunnel server on all
// of them would put an unnecessary listener on each machine.
func TestStartControlPlaneTunnel_NoOpWhenNotControlPlane(t *testing.T) {
	port := freePort(t)
	cases := map[string]*conf.FileConfig{
		"no cluster config": {AgentName: "worker"},
		"cluster but not control plane": {
			AgentName: "worker",
			Cluster:   &conf.ClusterConfig{TunnelBindPort: port},
		},
		"explicitly false": {
			AgentName: "worker",
			Cluster:   &conf.ClusterConfig{ControlPlane: boolPtr(false), TunnelBindPort: port},
		},
	}
	for name, fc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			g, gctx := errgroup.WithContext(ctx)

			startControlPlaneTunnel(gctx, g, fc, filepath.Join(t.TempDir(), "agent.json"))

			if waitDialable(fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond) {
				t.Fatal("a non-control-plane host started a tunnel server")
			}
			if fc.Cluster != nil && fc.Cluster.TunnelToken != "" {
				t.Error("minted a tunnel token on a host that hosts nothing")
			}
			cancel()
			_ = g.Wait()
		})
	}
}
