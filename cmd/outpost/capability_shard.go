package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent/shard"
	"github.com/qiangli/outpost/internal/agent/warm"
)

// Intra-home model sharding as an in-process capability (unit
// `provider: internal`, `owner: outpost`, `capability: outpost.shard`).
//
// Sharding serves a model no single box can hold by splitting it across
// mesh peers. Everything it needs is inside this process — the libp2p mesh
// forwarder it carries its RPC over, the peer-plane ring it discovers
// workers through, and the host's own mesh peer id — none of which a child
// process could be handed. So it is a capability, and the declaration owns
// what is decidable: `enabled`, and `settings.{role,peers,node_bytes,
// server_bin,worker_bin}` (the old cluster.shard.* block).
//
// A host with no mesh, no peer plane or no cloudbox pairing cannot shard,
// and an enabled unit on such a host now says SO — `outpost services`
// reports failed with the reason, where the old `ShardOn()` gate silently
// returned a nil manager and every shard command answered "not enabled on
// this host" without saying why.
type shardCapability struct {
	// Build constructs the manager from the declaration plus the host's
	// mesh/peer-plane wiring. It returns a reason when the host cannot
	// shard, which becomes the unit's activation refusal.
	Build func(s shardSettings) (*shard.Manager, error)
	// ServeBlobs exposes this node's on-disk model store over the mesh for
	// peer provisioning, returning the withdraw function. Nil, or a nil
	// cleanup, means the host has no mesh to expose it on.
	ServeBlobs func() (func(), error)

	run goroutineRunner

	mu    sync.Mutex
	mgr   *shard.Manager
	blobs func()
}

// errShardUnitInactive is what every shard verb answers while the unit is
// not active. The reason WHY lives on the unit's status row (`outpost
// services`), which is one place instead of a sentence repeated in each
// closure — and unlike the old message it is not a guess about which of
// pairing, mesh or the toggle is missing.
var errShardUnitInactive = errors.New("sharding is not active on this host — enable its service unit (`outpost builtins set --service shard=on`) and check `outpost services` for the reason")

// shardSettings is the declaration as the shard plane reads it.
type shardSettings struct {
	// Role is "auto" (default; most-VRAM host leads), "leader" or "worker".
	Role string
	// Peers selects worker hostnames; empty or "auto" means every
	// reachable same-vicinity mesh peer.
	Peers []string
	// NodeBytes is this node's model-memory budget (VRAM + usable RAM).
	// 0 lets the node measure its own capacity.
	NodeBytes uint64
	// ServerBin / WorkerBin are this node's Prima binaries. Empty lets the
	// node find its own.
	ServerBin string
	WorkerBin string
}

func readShardSettings(cfg service.Config) shardSettings {
	return shardSettings{
		Role:      setting(cfg, "role"),
		Peers:     settingList(cfg, "peers"),
		NodeBytes: settingUint(cfg, "node_bytes", 0),
		ServerBin: setting(cfg, "server_bin"),
		WorkerBin: setting(cfg, "worker_bin"),
	}
}

// Manager is the live shard manager, nil while the unit is not active. The
// shard CLI/MCP surfaces and the warm plane read it through here.
func (c *shardCapability) Manager() *shard.Manager {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mgr
}

// ShardControl is the warm plane's view. It returns a TYPED NIL-SAFE value:
// a nil *Manager must not become a non-nil warm.ShardControl, or the warm
// supervisor would try to form a shard through a manager that is not there.
func (c *shardCapability) ShardControl() warm.ShardControl {
	mgr := c.Manager()
	if mgr == nil {
		return nil
	}
	return mgr
}

func (c *shardCapability) Activate(ctx context.Context, cfg service.Config) error {
	if c.Build == nil {
		return fmt.Errorf("shard: no shard wiring on this host")
	}
	mgr, err := c.Build(readShardSettings(cfg))
	if err != nil {
		return err
	}
	if mgr == nil {
		return fmt.Errorf("shard: the mesh data plane and the peer plane must both be up (sharding rides them)")
	}
	var blobs func()
	if c.ServeBlobs != nil {
		cleanup, err := c.ServeBlobs()
		if err != nil {
			// Not fatal: a shard can still form, it just cannot hand a
			// peer a model it is missing without an ollama pull.
			slog.Warn("shard: serving the local model store over the mesh failed", "err", err)
		} else {
			blobs = cleanup
		}
	}
	c.mu.Lock()
	c.withdrawBlobsLocked()
	c.mgr, c.blobs = mgr, blobs
	c.mu.Unlock()
	c.run.start(ctx, func(runCtx context.Context) error {
		if err := mgr.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	})
	slog.Info("shard: participating in intra-home model sharding")
	return nil
}

// Configure rebuilds the manager: the ring, the budget and the binaries are
// read once at construction.
func (c *shardCapability) Configure(ctx context.Context, cfg service.Config) error {
	return c.Activate(ctx, cfg)
}

func (c *shardCapability) Deactivate(ctx context.Context) error {
	c.run.stop(ctx)
	c.mu.Lock()
	c.mgr = nil
	c.withdrawBlobsLocked()
	c.mu.Unlock()
	return nil
}

// withdrawBlobsLocked unexposes the model store. Called with c.mu held.
func (c *shardCapability) withdrawBlobsLocked() {
	if c.blobs != nil {
		c.blobs()
		c.blobs = nil
	}
}

func (c *shardCapability) Health(context.Context) error { return c.run.health("shard") }
