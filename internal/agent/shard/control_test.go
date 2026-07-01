package shard

import (
	"context"
	"fmt"
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
	// The worker stub carries no prima binaries, so make the readiness gate see
	// it as worker-capable (this test exercises the form path, not the gate).
	lm.ping = func(_ context.Context, p ShardPeer) (*StatusReport, error) {
		return &StatusReport{Host: p.Host, WorkerBin: true}, nil
	}
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

// TestReadyRing_DropsUnreadyPeers: a same-LAN peer that is reachable but has no
// worker binary (a Windows box), and a peer that's unreachable, both drop out;
// ranks re-number, so the form proceeds with only the shard-ready nodes instead
// of aborting or hanging the prima ring.
func TestReadyRing_DropsUnreadyPeers(t *testing.T) {
	m := NewManager(ManagerConfig{Self: ShardPeer{Host: "leader", PeerID: "L"}, Peers: &fakePeers{}})
	base := &Ring{Members: []Member{
		{Rank: 0, Host: "leader", PeerID: "L"},
		{Rank: 1, Host: "cap", PeerID: "N"},   // reachable + has worker binary → kept
		{Rank: 2, Host: "nobin", PeerID: "P"}, // reachable but no worker binary → dropped
		{Rank: 3, Host: "gone", PeerID: "G"},  // unreachable → dropped
	}}
	m.ping = func(_ context.Context, p ShardPeer) (*StatusReport, error) {
		switch p.Host {
		case "cap":
			return &StatusReport{Host: "cap", WorkerBin: true}, nil
		case "nobin":
			return &StatusReport{Host: "nobin", WorkerBin: false}, nil
		default:
			return nil, fmt.Errorf("unreachable")
		}
	}
	got := m.readyRing(context.Background(), base)
	if len(got.Members) != 2 {
		t.Fatalf("want 2 members (leader+cap), got %d: %+v", len(got.Members), got.Members)
	}
	if got.Members[0].Host != "leader" || got.Members[0].Rank != 0 {
		t.Errorf("rank0 = %+v, want leader/0", got.Members[0])
	}
	if got.Members[1].Host != "cap" || got.Members[1].Rank != 1 {
		t.Errorf("rank1 = %+v, want cap/1 (nobin+gone dropped, re-ranked)", got.Members[1])
	}
}

// TestReadyRing_NilBase: a nil candidate ring yields a self-only ring (the
// caller then treats <2 members as "no ready peers").
func TestReadyRing_NilBase(t *testing.T) {
	m := NewManager(ManagerConfig{Self: ShardPeer{Host: "leader", PeerID: "L"}, Peers: &fakePeers{}})
	got := m.readyRing(context.Background(), nil)
	if got == nil || len(got.Members) != 1 || got.Members[0].Host != "leader" {
		t.Fatalf("nil base → self-only ring, got %+v", got)
	}
}
