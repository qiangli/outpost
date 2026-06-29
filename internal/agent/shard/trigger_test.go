package shard

import (
	"context"
	"testing"
)

func TestMaybeShard_AutoTrigger(t *testing.T) {
	const GB = 1 << 30
	newM := func() (*Manager, *string) {
		m := NewManager(ManagerConfig{Self: ShardPeer{Host: "leader", PeerID: "self"}, Forwarder: newFake(), Peers: &fakePeers{}})
		triggered := new(string)
		m.orchestrate = func(_ context.Context, model string, _ int, _ []string) error {
			*triggered = model
			return nil
		}
		return m, triggered
	}
	withRing := func(m *Manager) {
		m.mu.Lock()
		m.ring = &Ring{Members: []Member{
			{Rank: 0, Host: "leader", PeerID: "self"},
			{Rank: 1, Host: "w", PeerID: "wpid"},
		}}
		m.mu.Unlock()
	}

	t.Run("too big + ring → orchestrate", func(t *testing.T) {
		m, tr := newM()
		withRing(m)
		if err := m.MaybeShard(context.Background(),
			[]LocalModel{{Name: "small", Bytes: 4 * GB}, {Name: "big", Bytes: 80 * GB}}, 24*GB, 11434); err != nil {
			t.Fatal(err)
		}
		if *tr != "big" {
			t.Errorf("triggered = %q, want big", *tr)
		}
	})

	t.Run("no ring → no trigger", func(t *testing.T) {
		m, tr := newM()
		_ = m.MaybeShard(context.Background(), []LocalModel{{Name: "big", Bytes: 80 * GB}}, 24*GB, 11434)
		if *tr != "" {
			t.Errorf("no-ring triggered %q", *tr)
		}
	})

	t.Run("fits locally → no trigger", func(t *testing.T) {
		m, tr := newM()
		withRing(m)
		_ = m.MaybeShard(context.Background(), []LocalModel{{Name: "fits", Bytes: 8 * GB}}, 24*GB, 11434)
		if *tr != "" {
			t.Errorf("fitting model triggered %q", *tr)
		}
	})

	t.Run("already active → no re-trigger", func(t *testing.T) {
		m, tr := newM()
		withRing(m)
		m.mu.Lock()
		m.activeModel = "big"
		m.mu.Unlock()
		_ = m.MaybeShard(context.Background(), []LocalModel{{Name: "big", Bytes: 80 * GB}}, 24*GB, 11434)
		if *tr != "" {
			t.Errorf("already-active re-triggered %q", *tr)
		}
	})
}
