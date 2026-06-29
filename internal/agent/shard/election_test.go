package shard

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// The election gathers peer capacity over a REAL mesh (via the ping) and the
// deterministic Decider picks the most-capacity leader — no human chooses. Here
// self has the larger budget, so self is elected and orchestrates.
func TestElect_SelfIsMostCapacity(t *testing.T) {
	const GB = 1 << 30
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

	// Worker: smaller budget (16GB), serving control so the ping resolves.
	wm := NewManager(ManagerConfig{
		Self:      ShardPeer{Host: "worker", PeerID: worker.PeerID()},
		Forwarder: worker.Forwarder(),
		Peers:     &fakePeers{},
		LocalLoad: func() ([]LocalModel, uint64) { return nil, 16 * GB },
	})
	cleanup, err := wm.ServeControl()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Leader: bigger budget (48GB); ring = self + worker.
	lm := NewManager(ManagerConfig{
		Self: ShardPeer{Host: "leader", PeerID: leader.PeerID()}, Forwarder: leader.Forwarder(), Peers: &fakePeers{},
	})
	lm.mu.Lock()
	lm.ring = &Ring{Members: []Member{
		{Rank: 0, Host: "leader", PeerID: leader.PeerID()},
		{Rank: 1, Host: "worker", PeerID: worker.PeerID()},
	}}
	lm.mu.Unlock()
	var ledModel string
	lm.orchestrate = func(_ context.Context, model string, _ int, _ []string) error {
		ledModel = model
		return nil
	}

	// A 60GB model: > leader's 48GB (so it triggers) and > the 48GB max (so it
	// shards), ≤ 64GB pooled. Election must pick leader (most capacity).
	if err := lm.autoShard(context.Background(), LocalModel{Name: "big", Bytes: 60 * GB}, 48*GB, 11434); err != nil {
		t.Fatalf("autoShard: %v", err)
	}
	if ledModel != "big" {
		t.Errorf("self (most capacity) was not elected to lead; ledModel=%q", ledModel)
	}
}

// When a peer has more capacity, this node defers (no self-orchestrate) — the
// elected peer leads via its own trigger.
func TestElect_DefersToBiggerPeer(t *testing.T) {
	const GB = 1 << 30
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

	// Peer "worker" actually has the bigger budget (96GB).
	wm := NewManager(ManagerConfig{
		Self:      ShardPeer{Host: "worker", PeerID: worker.PeerID()},
		Forwarder: worker.Forwarder(),
		Peers:     &fakePeers{},
		LocalLoad: func() ([]LocalModel, uint64) { return nil, 96 * GB },
	})
	cleanup, err := wm.ServeControl()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	lm := NewManager(ManagerConfig{
		Self: ShardPeer{Host: "leader", PeerID: leader.PeerID()}, Forwarder: leader.Forwarder(), Peers: &fakePeers{},
	})
	lm.mu.Lock()
	lm.ring = &Ring{Members: []Member{
		{Rank: 0, Host: "leader", PeerID: leader.PeerID()},
		{Rank: 1, Host: "worker", PeerID: worker.PeerID()},
	}}
	lm.mu.Unlock()
	fired := false
	lm.orchestrate = func(context.Context, string, int, []string) error { fired = true; return nil }

	// 60GB > this node's 40GB budget (triggers); worker (96GB) is the bigger leader.
	if err := lm.autoShard(context.Background(), LocalModel{Name: "big", Bytes: 60 * GB}, 40*GB, 11434); err != nil {
		t.Fatalf("autoShard: %v", err)
	}
	if fired {
		t.Error("this node self-orchestrated despite a more-capable peer being elected")
	}
}
