package shard

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// A leader pings a worker's shard-control /status over a REAL mesh and gets its
// readiness back — the app-level ping that replaces ssh-into-the-peer.
func TestPingPeer_OverMesh(t *testing.T) {
	worker := newMeshHost(t)
	defer worker.Close()
	leader := newMeshHost(t)
	defer leader.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := leader.LibP2PHost().Connect(ctx, peer.AddrInfo{
		ID:    worker.LibP2PHost().ID(),
		Addrs: worker.LibP2PHost().Addrs(),
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Worker: a model + budget + an active shard, serving control.
	wm := NewManager(ManagerConfig{
		Self:      ShardPeer{Host: "worker", PeerID: worker.PeerID()},
		Forwarder: worker.Forwarder(),
		Peers:     &fakePeers{},
		LocalLoad: func() ([]LocalModel, uint64) {
			return []LocalModel{{Name: "big", Bytes: 80 << 30}}, 48 << 30
		},
	})
	wm.mu.Lock()
	wm.activeModel = "big"
	wm.mu.Unlock()
	cleanup, err := wm.ServeControl()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Leader pings the worker — no ssh, no elevation.
	lm := NewManager(ManagerConfig{
		Self: ShardPeer{Host: "leader", PeerID: leader.PeerID()}, Forwarder: leader.Forwarder(), Peers: &fakePeers{},
	})
	rep, err := lm.PingPeer(ctx, ShardPeer{Host: "worker", PeerID: worker.PeerID()})
	if err != nil {
		t.Fatalf("PingPeer: %v", err)
	}
	if rep.Host != "worker" {
		t.Errorf("host = %q, want worker", rep.Host)
	}
	if rep.ActiveModel != "big" {
		t.Errorf("active model = %q, want big", rep.ActiveModel)
	}
	if rep.BudgetBytes != 48<<30 {
		t.Errorf("budget = %d, want %d", rep.BudgetBytes, uint64(48)<<30)
	}
	if len(rep.Models) != 1 || rep.Models[0].Name != "big" {
		t.Errorf("models = %+v", rep.Models)
	}
}

// LocalStatus reflects the node's own configured budget + binaries (no network).
func TestLocalStatus(t *testing.T) {
	m := NewManager(ManagerConfig{
		Self:      ShardPeer{Host: "n1", PeerID: "p1"},
		Forwarder: newFake(),
		Peers:     &fakePeers{},
		Bins:      ServeBins{ServerBin: "/no/such/server", WorkerBin: "/no/such/worker"},
		LocalLoad: func() ([]LocalModel, uint64) { return []LocalModel{{Name: "m", Bytes: 1 << 30}}, 8 << 30 },
	})
	s := m.LocalStatus()
	if s.Host != "n1" || s.BudgetBytes != 8<<30 || len(s.Models) != 1 {
		t.Errorf("unexpected status: %+v", s)
	}
	if s.ServerBin || s.WorkerBin {
		t.Errorf("nonexistent bins reported present: %+v", s)
	}
}
