package shard

import (
	"context"
	"testing"
	"time"
)

type fakePeers struct {
	peers []ShardPeer
	err   error
}

func (f *fakePeers) SameLANPeers(context.Context) ([]ShardPeer, error) {
	return f.peers, f.err
}

func TestManager_BuildRing_SelfPlusSortedPeers(t *testing.T) {
	m := NewManager(ManagerConfig{
		Self: ShardPeer{Host: "leader", PeerID: "self-pid"},
		Peers: &fakePeers{peers: []ShardPeer{
			{Host: "worker-b", PeerID: "pid-b"},
			{Host: "worker-a", PeerID: "pid-a"},
		}},
	})
	ring, err := m.buildRing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ring == nil || len(ring.Members) != 3 {
		t.Fatalf("expected a 3-member ring, got %+v", ring)
	}
	// rank 0 = self; peers follow in stable host order.
	want := []Member{
		{Rank: 0, Host: "leader", PeerID: "self-pid"},
		{Rank: 1, Host: "worker-a", PeerID: "pid-a"},
		{Rank: 2, Host: "worker-b", PeerID: "pid-b"},
	}
	for i, w := range want {
		if ring.Members[i] != w {
			t.Errorf("member %d = %+v, want %+v", i, ring.Members[i], w)
		}
	}
	// The built ring is launch-ready: PlanFor succeeds for every rank.
	for r := 0; r < 3; r++ {
		if _, err := ring.PlanFor(r); err != nil {
			t.Errorf("PlanFor(%d) on the built ring: %v", r, err)
		}
	}
}

func TestManager_BuildRing_NoPeers_NoRing(t *testing.T) {
	m := NewManager(ManagerConfig{
		Self:  ShardPeer{Host: "solo", PeerID: "pid"},
		Peers: &fakePeers{peers: nil},
	})
	ring, err := m.buildRing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ring != nil {
		t.Errorf("a solo node can't shard; expected nil ring, got %+v", ring)
	}
}

func TestManager_Run_PopulatesAndStops(t *testing.T) {
	m := NewManager(ManagerConfig{
		Self:     ShardPeer{Host: "leader", PeerID: "self"},
		Peers:    &fakePeers{peers: []ShardPeer{{Host: "w", PeerID: "wpid"}}},
		Interval: 10 * time.Millisecond,
	})
	if m.Ring() != nil {
		t.Fatal("ring should be nil before Run")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for m.Ring() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if m.Ring() == nil {
		t.Fatal("Run did not populate the candidate ring")
	}
	if got := len(m.Ring().Members); got != 2 {
		t.Errorf("expected 2 members (self + worker), got %d", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on ctx cancel")
	}
}
