package shard

import (
	"context"
	"testing"
)

// The discover loop's refresh auto-shards a too-big local model when a LocalLoad
// source is wired (the daemon feed).
func TestRefresh_AutoShards(t *testing.T) {
	m := NewManager(ManagerConfig{
		Self:      ShardPeer{Host: "leader", PeerID: "self"},
		Forwarder: newFake(),
		Peers:     &fakePeers{peers: []ShardPeer{{Host: "w", PeerID: "wpid"}}},
		LocalLoad: func() ([]LocalModel, uint64) {
			return []LocalModel{{Name: "big", Bytes: 80 << 30}}, 24 << 30
		},
	})
	var triggered string
	m.orchestrate = func(_ context.Context, model string, _ int, _ []string) error {
		triggered = model
		return nil
	}
	m.gather = func(context.Context, uint64, uint64) ([]NodeCapacity, map[string]ShardPeer) {
		return []NodeCapacity{{Host: "leader", Bytes: 60 << 30}, {Host: "w", Bytes: 40 << 30}}, nil
	}

	m.refresh(context.Background())

	if triggered != "big" {
		t.Errorf("refresh did not auto-shard the too-big model; triggered = %q", triggered)
	}
}

// No LocalLoad source → no auto-trigger (the safe default).
func TestRefresh_NoFeed_NoTrigger(t *testing.T) {
	m := NewManager(ManagerConfig{
		Self:      ShardPeer{Host: "leader", PeerID: "self"},
		Forwarder: newFake(),
		Peers:     &fakePeers{peers: []ShardPeer{{Host: "w", PeerID: "wpid"}}},
	})
	fired := false
	m.orchestrate = func(context.Context, string, int, []string) error { fired = true; return nil }
	m.refresh(context.Background())
	if fired {
		t.Error("auto-trigger fired without a LocalLoad source")
	}
}
