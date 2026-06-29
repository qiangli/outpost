package shard

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// TestOrchestrate_TellsWorkersOverMesh: a leader orchestrating a shard reaches
// the worker's shard-control endpoint over a REAL mesh and hands it the right
// ring/rank/model. (The worker records via onForm instead of launching, so the
// in-process two-rank port clash never happens — that's the cross-machine path.)
func TestOrchestrate_TellsWorkersOverMesh(t *testing.T) {
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

	// Worker: serve the control endpoint, recording form requests.
	wm := NewManager(ManagerConfig{
		Self: ShardPeer{Host: "worker", PeerID: worker.PeerID()}, Forwarder: worker.Forwarder(), Peers: &fakePeers{},
	})
	var rec struct {
		sync.Mutex
		ring  *Ring
		rank  int
		model string
	}
	wm.onForm = func(_ context.Context, ring *Ring, rank int, sc ServeConfig) error {
		rec.Lock()
		defer rec.Unlock()
		rec.ring, rec.rank, rec.model = ring, rank, sc.Model
		return nil
	}
	cleanup, err := wm.ServeControl()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Leader: a ring of self (rank 0) + the worker (rank 1); stub its own form.
	lm := NewManager(ManagerConfig{
		Self: ShardPeer{Host: "leader", PeerID: leader.PeerID()}, Forwarder: leader.Forwarder(), Peers: &fakePeers{},
	})
	lm.mu.Lock()
	lm.ring = &Ring{Members: []Member{
		{Rank: 0, Host: "leader", PeerID: leader.PeerID()},
		{Rank: 1, Host: "worker", PeerID: worker.PeerID()},
	}}
	lm.mu.Unlock()
	leaderFormed := false
	lm.onForm = func(_ context.Context, _ *Ring, rank int, _ ServeConfig) error {
		if rank == 0 {
			leaderFormed = true
		}
		return nil
	}

	if err := lm.Orchestrate(context.Background(), "qwen-72b", 11434, []string{"--prefetch"}); err != nil {
		t.Fatalf("Orchestrate: %v", err)
	}
	if !leaderFormed {
		t.Error("leader did not form its own rank 0")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		rec.Lock()
		done := rec.model != ""
		rec.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec.Lock()
	defer rec.Unlock()
	if rec.model != "qwen-72b" {
		t.Errorf("worker model = %q, want qwen-72b", rec.model)
	}
	if rec.rank != 1 {
		t.Errorf("worker rank = %d, want 1", rec.rank)
	}
	if rec.ring == nil || len(rec.ring.Members) != 2 {
		t.Errorf("worker ring = %+v", rec.ring)
	}
}
