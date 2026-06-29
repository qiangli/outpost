package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/mesh"
	"github.com/qiangli/outpost/internal/agent/ollama"
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
	var bins shard.ServeBins
	var nodeBytes uint64
	if fc.Shard != nil {
		bins = shard.ServeBins{ServerBin: fc.Shard.ServerBin, WorkerBin: fc.Shard.WorkerBin}
		nodeBytes = fc.Shard.NodeBytes
	}
	return shard.NewManager(shard.ManagerConfig{
		Self:      shard.ShardPeer{Host: fc.AgentName, PeerID: meshHost.PeerID()},
		Forwarder: meshHost.Forwarder(),
		Peers:     disc,
		Bins:      bins,
		LocalLoad: func() ([]shard.LocalModel, uint64) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return ollamaLocalModels(ctx, "http://127.0.0.1:11434"), nodeBytes
		},
	})
}

// ollamaLocalModels queries the local ollama /api/tags for this node's models +
// on-disk sizes (best-effort; empty on any error). The budget pairs with it in
// LocalLoad to drive the auto-trigger.
func ollamaLocalModels(ctx context.Context, ollamaURL string) []shard.LocalModel {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ollamaURL+"/api/tags", nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		Models []struct {
			Name string `json:"name"`
			Size uint64 `json:"size"`
		} `json:"models"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil
	}
	models := make([]shard.LocalModel, 0, len(out.Models))
	for _, mi := range out.Models {
		models = append(models, shard.LocalModel{Name: mi.Name, Bytes: mi.Size})
	}
	return models
}

// shardClusterSource composes the existing cluster source (if any) with the
// shard manager's actively-served model, so the LLM-pool registry push
// advertises a sharded model and cloudbox's existing routing/load-balancing
// sends requests for it to this (leader) node — sharding fuses into the pool.
type shardClusterSource struct {
	base ollama.ClusterSource // existing source (e.g. clusterllm); may be nil
	mgr  *shard.Manager
}

func (s shardClusterSource) ClusterSnapshot() *ollama.ClusterCapacity {
	if s.base != nil {
		return s.base.ClusterSnapshot()
	}
	return nil
}

func (s shardClusterSource) ClusterModels() []ollama.ModelInfo {
	var models []ollama.ModelInfo
	if cms, ok := s.base.(ollama.ClusterModelSource); ok {
		models = cms.ClusterModels()
	}
	if name := s.mgr.ActiveModel(); name != "" {
		models = append(models, ollama.ModelInfo{Name: name})
	}
	return models
}
