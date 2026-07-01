package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/mesh"
	"github.com/qiangli/outpost/internal/agent/ollama"
	"github.com/qiangli/outpost/internal/agent/peerplane"
	"github.com/qiangli/outpost/internal/agent/peerstatus"
	"github.com/qiangli/outpost/internal/agent/shard"
	"github.com/qiangli/outpost/internal/agent/sysinfo"
)

// shardCmd is the MCP-client CLI for the libp2p-mesh shard control plane: tell a
// peer to LEAD a shard for a model (no ssh), and read a node's shard readiness.
func shardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shard",
		Short: "Drive intra-LAN model sharding over the libp2p mesh (no ssh)",
		Long: `Trigger a paired peer to LEAD a shard for a model — the leader self-provisions
and orchestrates its same-LAN ring over the mesh — and inspect a node's shard
readiness. Both subcommands drive the local daemon over MCP.`,
	}
	cmd.AddCommand(shardTriggerCmd(), shardStatusCmd(), shardLogCmd())
	return cmd
}

func shardTriggerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "trigger <host> <model>",
		Short: "Tell a peer host to LEAD a shard for a model over the mesh",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out struct {
				OK bool `json:"ok"`
			}
			if err := runShardTool(cmd.Context(), "outpost_shard_trigger",
				map[string]string{"host": args[0], "model": args[1]}, &out); err != nil {
				return err
			}
			fmt.Printf("told %s to lead a shard for %s\n", args[0], args[1])
			return nil
		},
	}
}

func shardStatusCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status [host]",
		Short: "Show this node's (or a peer's) shard readiness over the mesh",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := ""
			if len(args) == 1 {
				host = args[0]
			}
			var out struct {
				Status *shard.StatusReport `json:"status"`
			}
			if err := runShardTool(cmd.Context(), "outpost_shard_status",
				map[string]string{"host": host}, &out); err != nil {
				return err
			}
			if jsonOut {
				b, _ := json.MarshalIndent(out.Status, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			r := out.Status
			if r == nil {
				fmt.Println("no status")
				return nil
			}
			fmt.Printf("host:          %s\n", r.Host)
			fmt.Printf("budget_bytes:  %d\n", r.BudgetBytes)
			fmt.Printf("server_bin:    %v\n", r.ServerBin)
			fmt.Printf("worker_bin:    %v\n", r.WorkerBin)
			fmt.Printf("active_model:  %s\n", r.ActiveModel)
			fmt.Printf("ring_members:  %d\n", r.RingMembers)
			if r.LastExit != "" {
				fmt.Printf("last_exit:     %s\n", r.LastExit)
			}
			for _, m := range r.Models {
				fmt.Printf("  model %s (%d bytes)\n", m.Name, m.Bytes)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON")
	return cmd
}

func shardLogCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "log [host]",
		Short: "Tail this node's (or a peer's) captured prima-rank shard logs over the mesh",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := ""
			if len(args) == 1 {
				host = args[0]
			}
			var out struct {
				Log string `json:"log"`
			}
			if err := runShardTool(cmd.Context(), "outpost_shard_log",
				map[string]string{"host": host}, &out); err != nil {
				return err
			}
			if out.Log == "" {
				fmt.Println("no shard logs")
				return nil
			}
			fmt.Print(out.Log)
			return nil
		},
	}
}

func runShardTool(ctx context.Context, name string, args, out any) error {
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	return session.callTool(ctx, name, args, out)
}

// peerPlaneDiscoverer adapts the peer-plane (same-LAN tier filter) + cloudbox
// peer/connect (libp2p-id resolution) to the shard manager's PeerDiscoverer.
type peerPlaneDiscoverer struct {
	svc       *peerplane.Service
	client    *peerplane.Client
	selfHost  string
	mesh      *mesh.Host
	rdvPeerID func(host string) string // host → peer id from the rendezvous (reliable), nil-ok

	platMu    sync.Mutex
	platCache map[string]string // host → "goos/goarch", last-good (survives cloudbox blips)

	idMu    sync.Mutex
	idCache map[string]string // host → libp2p peer id, last-good (survives rendezvous blips)
}

func (d *peerPlaneDiscoverer) SameLANPeers(ctx context.Context) ([]shard.ShardPeer, error) {
	// Engine-compatibility gate: a worker runs a prima engine built for its own
	// platform, and today the gpt-oss engine ships only for the leader's platform
	// — so a same-LAN box on a different OS/arch (e.g. a Windows peer that shares
	// this host's IPv6 /64) can't run a rank and would only poison the form. Drop
	// it here, before it ever enters the candidate ring. (When per-platform engines
	// ship, this should gate on a reported engine version instead of the leader's
	// platform.) Fail-open when the platform is unknown so discovery still works.
	plat := d.peerPlatforms(ctx)
	selfPlat := runtime.GOOS + "/" + runtime.GOARCH
	var peers []shard.ShardPeer
	for _, t := range d.svc.Snapshot() {
		if p, ok := plat[t.Host]; ok && p != selfPlat {
			slog.Debug("shard discover: drop incompatible platform", "host", t.Host, "plat", p, "self", selfPlat)
			continue // incompatible platform — can't run the engine
		}
		// Resolve the host's libp2p peer id. Prefer the rendezvous's own
		// host→peer-id map — the id the mesh actually connected with — because the
		// per-call cloudbox peer/connect can momentarily return an empty id on a
		// stale announce even while a direct mesh link is up. Fall back to a fresh
		// cloudbox resolve, then to the last-good cached id.
		peerID := ""
		if d.rdvPeerID != nil {
			peerID = d.rdvPeerID(t.Host)
		}
		if peerID == "" {
			if target, err := d.client.Connect(ctx, d.selfHost, t.Host); err == nil && target != nil {
				peerID = target.Peer.PeerID
			}
		}
		if peerID == "" {
			peerID = d.cachedPeerID(t.Host)
		}
		if peerID == "" {
			continue // never resolved a libp2p id for this host → skip
		}
		d.rememberPeerID(t.Host, peerID)
		// Fast local link if the peerplane RTT-tiered it LAN/TP, OR the mesh holds
		// a DIRECT connection over a private/link-local address. The latter rescues
		// link-local (a direct wired link) + firewalled LANs the UDP prober reports "unreached"
		// — the mesh connection's own remote address is the ground truth.
		local := t.Tier == peerplane.TierLAN || t.Tier == peerplane.TierTP
		meshCls := ""
		if d.mesh != nil {
			meshCls = d.mesh.PeerLinkClass(peerID)
			if meshCls == "tp" || meshCls == "lan" {
				local = true
			}
		}
		slog.Debug("shard discover: peer eval", "host", t.Host, "tier", t.Tier, "mesh_class", meshCls, "local", local)
		if !local {
			continue // sharding rides a fast local link only
		}
		peers = append(peers, shard.ShardPeer{Host: t.Host, PeerID: peerID})
	}
	return peers, nil
}

// cachedPeerID returns the last-good libp2p peer id resolved for host, or "".
func (d *peerPlaneDiscoverer) cachedPeerID(host string) string {
	d.idMu.Lock()
	defer d.idMu.Unlock()
	return d.idCache[host]
}

// rememberPeerID records the libp2p peer id resolved for host, so a later
// rendezvous blip that returns no id can fall back to it.
func (d *peerPlaneDiscoverer) rememberPeerID(host, id string) {
	d.idMu.Lock()
	defer d.idMu.Unlock()
	if d.idCache == nil {
		d.idCache = map[string]string{}
	}
	d.idCache[host] = id
}

// peerPlatforms returns host → "goos/goarch" from cloudbox's peer status board
// (the OS/arch each host last reported). Used by SameLANPeers to keep only
// engine-compatible workers in the shard ring. Returns an empty map on any fetch
// failure so the platform gate fails OPEN (no filter) rather than blocking a
// shard when cloudbox is briefly unreachable.
func (d *peerPlaneDiscoverer) peerPlatforms(ctx context.Context) map[string]string {
	d.platMu.Lock()
	defer d.platMu.Unlock()
	if d.platCache == nil {
		d.platCache = map[string]string{}
	}
	// Merge a fresh fetch into the cache; on failure we keep the last-good map so
	// a transient cloudbox blip (e.g. a tunnel reconnect) can't disable the gate
	// and let an incompatible peer back into the ring. A host's OS/arch is stable.
	if ps, err := peerstatus.Fetch(ctx, d.client.BaseURL, d.client.Token, d.client.HC); err == nil {
		for _, p := range ps {
			if p.OS != "" && p.Arch != "" {
				d.platCache[p.Host] = p.OS + "/" + p.Arch
			}
		}
	}
	out := make(map[string]string, len(d.platCache))
	for k, v := range d.platCache {
		out[k] = v
	}
	return out
}

// newShardManager builds the shard manager when sharding is on and the mesh +
// peer-plane are both up; nil otherwise (the daemon then starts nothing).
func newShardManager(fc *conf.FileConfig, meshHost *mesh.Host, peerSvc *peerplane.Service, rdv *mesh.Rendezvous) *shard.Manager {
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
		mesh:     meshHost,
	}
	if rdv != nil {
		disc.rdvPeerID = rdv.PeerIDForHost
	}
	var bins shard.ServeBins
	var nodeBytes uint64
	if fc.Shard != nil {
		bins = shard.ServeBins{ServerBin: fc.Shard.ServerBin, WorkerBin: fc.Shard.WorkerBin}
		nodeBytes = fc.Shard.NodeBytes
	}
	if nodeBytes == 0 {
		nodeBytes = detectShardBudget() // zero-config: the node measures its own capacity
	}
	if bins.ServerBin == "" {
		bins = defaultPrimaBins() // zero-config: the node finds its own prima binaries
	}
	return shard.NewManager(shard.ManagerConfig{
		Self:      shard.ShardPeer{Host: fc.AgentName, PeerID: meshHost.PeerID()},
		Forwarder: meshHost.Forwarder(),
		Peers:     disc,
		Bins:      bins,
		LogDir:    filepath.Dir(bins.ServerBin), // prima stdout+stderr → <prima dir>/prima-rank<N>.log
		Provision: func(ctx context.Context, name string) (string, error) {
			// Tier-(b) of ensureModel: fetch a missing model's GGUF from a same-LAN
			// mesh peer over the forwarder before falling back to an ollama pull.
			return provisionShard(ctx, bins, name, meshHost.Forwarder(), disc)
		},
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

// detectShardBudget measures this node's model-memory budget with no human
// config: summed discrete-GPU VRAM when present, else ~70% of system RAM (the
// safe fraction for unified-memory Apple Silicon and CPU hosts). This is what
// makes the auto-trigger + capacity election truly zero-config — the node
// measures its own hardware instead of being told.
func detectShardBudget() uint64 {
	info := sysinfo.Collect("")
	var vram uint64
	for _, g := range info.GPUs {
		if !g.UnifiedMemory {
			vram += g.VRAMTotalBytes
		}
	}
	if vram > 0 {
		return vram
	}
	return info.MemTotalBytes / 10 * 7
}

// defaultPrimaBins is the zero-config location the daemon looks for the prima
// binaries when no path is configured: <cache>/outpost/prima/llama-{server,cli}.
// Fleet upgrade (or binmgr, later) drops them here; the node finds them itself.
func defaultPrimaBins() shard.ServeBins {
	dir, err := conf.DefaultCacheDir()
	if err != nil {
		return shard.ServeBins{}
	}
	base := filepath.Join(dir, "prima")
	srv := filepath.Join(base, "llama-server")
	wrk := filepath.Join(base, "llama-cli")
	if runtime.GOOS == "windows" {
		srv += ".exe"
		wrk += ".exe"
	}
	return shard.ServeBins{ServerBin: srv, WorkerBin: wrk}
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
