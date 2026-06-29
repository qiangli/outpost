package main

import (
	"context"
	"net/http"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/mesh"
	"github.com/qiangli/outpost/internal/agent/peerplane"
	"github.com/qiangli/outpost/internal/agent/shard"
)

// peerPlaneDiscoverer adapts the peer-plane (same-LAN tier filter) + cloudbox
// peer/connect (libp2p-id resolution) to the shard manager's PeerDiscoverer.
type peerPlaneDiscoverer struct {
	svc      *peerplane.Service
	client   *peerplane.Client
	selfHost string
}

func (d *peerPlaneDiscoverer) SameLANPeers(ctx context.Context) ([]shard.ShardPeer, error) {
	var peers []shard.ShardPeer
	for _, t := range d.svc.Snapshot() {
		if t.Tier != peerplane.TierLAN && t.Tier != peerplane.TierTP {
			continue // sharding rides a fast local link only
		}
		target, err := d.client.Connect(ctx, d.selfHost, t.Host)
		if err != nil || target == nil || target.Peer.PeerID == "" {
			continue // can't resolve a libp2p id → skip
		}
		peers = append(peers, shard.ShardPeer{Host: t.Host, PeerID: target.Peer.PeerID})
	}
	return peers, nil
}

// newShardManager builds the shard manager when sharding is on and the mesh +
// peer-plane are both up; nil otherwise (the daemon then starts nothing).
func newShardManager(fc *conf.FileConfig, meshHost *mesh.Host, peerSvc *peerplane.Service) *shard.Manager {
	if !fc.ShardOn() || meshHost == nil || peerSvc == nil {
		return nil
	}
	cb := cloudboxHTTPBase(fc)
	if cb == "" {
		return nil
	}
	disc := &peerPlaneDiscoverer{
		svc:      peerSvc,
		client:   &peerplane.Client{BaseURL: cb, Token: fc.AccessToken, HC: &http.Client{Timeout: 10 * time.Second}},
		selfHost: fc.AgentName,
	}
	return shard.NewManager(shard.ManagerConfig{
		Self:      shard.ShardPeer{Host: fc.AgentName, PeerID: meshHost.PeerID()},
		Forwarder: meshHost.Forwarder(),
		Peers:     disc,
	})
}
