package shard

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// ShardPeer identifies a shard-ring participant on the mesh: a hostname label
// plus its libp2p peer id (what the forwarder dials).
type ShardPeer struct {
	Host   string
	PeerID string
}

// PeerDiscoverer yields the reachable same-LAN owner peers eligible as shard
// workers. The real implementation wraps the peer-plane (same-LAN/tier filter)
// + cloudbox peer/connect (peer-id resolution); tests inject a fake.
type PeerDiscoverer interface {
	SameLANPeers(ctx context.Context) ([]ShardPeer, error)
}

// ManagerConfig configures the shard manager.
type ManagerConfig struct {
	Self      ShardPeer      // this host (label + its own libp2p peer id)
	Forwarder Forwarder      // the mesh forwarder (the data plane)
	Peers     PeerDiscoverer // same-LAN owner-peer source
	Interval  time.Duration  // discover cadence (0 → 30s)
	Logger    *slog.Logger
}

// Manager keeps a current candidate shard Ring up to date: it periodically
// discovers the reachable same-LAN owner peers and assembles a launch-ready
// ring. It does NOT form a shard by itself — standing the ring up is gated on a
// too-big model (the auto-trigger, v1d); the manager just keeps the ring ready.
type Manager struct {
	self     ShardPeer
	fwd      Forwarder
	peers    PeerDiscoverer
	interval time.Duration
	log      *slog.Logger

	mu   sync.Mutex
	ring *Ring
}

// NewManager builds a shard manager. Defaults: 30s discover interval, the
// default slog logger.
func NewManager(cfg ManagerConfig) *Manager {
	interval := cfg.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		self:     cfg.Self,
		fwd:      cfg.Forwarder,
		peers:    cfg.Peers,
		interval: interval,
		log:      log,
	}
}

// Run refreshes the candidate ring immediately, then on every interval, until
// ctx is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	m.refresh(ctx)
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			m.refresh(ctx)
		}
	}
}

func (m *Manager) refresh(ctx context.Context) {
	ring, err := m.buildRing(ctx)
	if err != nil {
		m.log.Debug("shard: discover failed", "err", err)
		return
	}
	m.mu.Lock()
	prev := m.ring
	m.ring = ring
	m.mu.Unlock()
	if ring != nil && (prev == nil || len(prev.Members) != len(ring.Members)) {
		m.log.Info("shard: candidate ring", "members", len(ring.Members), "leader", m.self.Host)
	}
}

// buildRing discovers same-LAN owner peers and assembles a candidate Ring: this
// host as rank 0 (the leader placeholder — v1d re-picks by VRAM) plus the peers
// in stable host order. Returns nil when there are no peers (a one-node "ring"
// can't shard).
func (m *Manager) buildRing(ctx context.Context) (*Ring, error) {
	peers, err := m.peers.SameLANPeers(ctx)
	if err != nil {
		return nil, err
	}
	if len(peers) == 0 {
		return nil, nil
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Host < peers[j].Host })

	members := make([]Member, 0, len(peers)+1)
	members = append(members, Member{Rank: 0, Host: m.self.Host, PeerID: m.self.PeerID})
	for i, p := range peers {
		members = append(members, Member{Rank: i + 1, Host: p.Host, PeerID: p.PeerID})
	}
	return &Ring{Members: members}, nil
}

// Ring returns a snapshot of the current candidate ring (nil if there are no
// same-LAN peers to shard with).
func (m *Manager) Ring() *Ring {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ring
}
