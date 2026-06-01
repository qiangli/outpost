// Roadmap item #17 — memberlist SWIM gossip layer.
//
// Without gossip, each outpost only sees the edges IT dialed
// directly. The reachability ledger surfaces those local edges,
// but `outpost peers route-to` is severely limited: a peer that
// every fleet member CAN reach but THIS box can't dial directly
// is invisible.
//
// With gossip, the fleet shares its edge views. Every outpost
// broadcasts (and merges) a compact summary of (a) which peers it
// knows about and (b) the freshness of its most-recent observation
// of each. The discovery Cache absorbs gossiped peers with
// SourceGossip, so route-to can now use second-hand reachability
// to suggest paths.
//
// Implementation: hashicorp/memberlist (SWIM failure detection +
// gossip plumbing). One Member per outpost; Delegate publishes /
// merges Cache snapshots; bootstrap joins come from mDNS and the
// cloudbox NAT-locality hints we already collect.

package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
)

// DefaultGossipBindPort is the SWIM UDP/TCP port memberlist binds.
// Operator can override via FileConfig.GossipBindAddr (host:port).
// 7946 is memberlist's default — picked deliberately to be the
// recognized "SWIM" port for firewall-allow rules.
const DefaultGossipBindPort = 7946

// gossipBroadcastTTL is how long a gossiped Peer should remain in
// the cache before being treated as stale. Longer than the
// 5-min observation tick (so gossip survives a tick miss); shorter
// than the Cache TTL (so non-refreshed gossip-only peers age out).
const gossipBroadcastTTL = 10 * time.Minute

// GossipConfig is the input to NewGossip.
type GossipConfig struct {
	// SelfPeerID identifies this outpost on the gossip mesh.
	SelfPeerID PeerID

	// SelfAgentName is the human-friendly name (PeerID is opaque).
	SelfAgentName string

	// BindAddr is the local listen address (host:port). Empty
	// uses 0.0.0.0:DefaultGossipBindPort.
	BindAddr string

	// AdvertiseAddr is the address gossip messages claim as the
	// source. Defaults to BindAddr's host. Override when a NAT
	// rewrites the source IP.
	AdvertiseAddr string

	// Cache is the live discovery cache. Gossip-received peers
	// get Upserted here with SourceGossip.
	Cache *Cache

	// Bootstrap is a callback returning a list of join addresses
	// (host:port). Called at boot AND on the periodic re-bootstrap
	// ticker so newly-discovered peers (via mDNS or NAT hints)
	// get pulled into the mesh. May return an empty list — gossip
	// still works as a single-node sink that accepts pushes.
	Bootstrap func() []string

	// Logger is optional; nil = slog-default.
	Logger io.Writer
}

// Gossip is the lifecycle handle. Construct via NewGossip; call
// Run to start the SWIM listener + bootstrap loop; closes on ctx.
type Gossip struct {
	cfg  GossipConfig
	ml   *memberlist.Memberlist
	mu   sync.RWMutex
	last time.Time // last successful local-state push
}

// NewGossip constructs a gossip handle. Does NOT start the listener
// — call Run.
func NewGossip(cfg GossipConfig) (*Gossip, error) {
	if cfg.Cache == nil {
		return nil, fmt.Errorf("gossip: nil Cache")
	}
	if cfg.SelfPeerID == "" {
		return nil, fmt.Errorf("gossip: empty SelfPeerID")
	}
	return &Gossip{cfg: cfg}, nil
}

// Run starts memberlist + the bootstrap loop. Blocks until
// ctx.Done().
func (g *Gossip) Run(ctx context.Context) error {
	bindHost, bindPort, err := splitBindAddr(g.cfg.BindAddr)
	if err != nil {
		return fmt.Errorf("gossip: parse bind addr: %w", err)
	}

	mlCfg := memberlist.DefaultLANConfig()
	mlCfg.Name = string(g.cfg.SelfPeerID)
	mlCfg.BindAddr = bindHost
	mlCfg.BindPort = bindPort
	if g.cfg.AdvertiseAddr != "" {
		advHost, advPort, err := splitBindAddr(g.cfg.AdvertiseAddr)
		if err != nil {
			return fmt.Errorf("gossip: parse advertise addr: %w", err)
		}
		mlCfg.AdvertiseAddr = advHost
		mlCfg.AdvertisePort = advPort
	}
	if g.cfg.Logger != nil {
		// memberlist uses log.Logger, not slog. Redirect output to
		// the writer the caller passed (typically io.Discard in
		// tests, os.Stderr otherwise).
		mlCfg.LogOutput = g.cfg.Logger
	}
	// Delegate publishes and merges PeerSummary blobs.
	del := &gossipDelegate{g: g}
	mlCfg.Delegate = del
	// Event delegate observes join/leave so we can log failure
	// detection and (Wave 3B.4) feed the active/passive promotion.
	mlCfg.Events = &gossipEventDelegate{g: g}

	ml, err := memberlist.Create(mlCfg)
	if err != nil {
		return fmt.Errorf("gossip: memberlist.Create: %w", err)
	}
	g.ml = ml
	slog.Info("gossip: listening", "bind", fmt.Sprintf("%s:%d", bindHost, bindPort), "name", mlCfg.Name)
	defer ml.Shutdown()

	// Initial bootstrap; then every 60s to absorb peers that came
	// into view since last attempt (mDNS late-join, NAT hint poll).
	g.bootstrap()
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			g.bootstrap()
		}
	}
}

// bootstrap pulls fresh addresses from Bootstrap() and tries to
// join those that aren't already members. Failures are logged but
// not fatal — gossip works as a single-node sink even before
// bootstrap succeeds.
func (g *Gossip) bootstrap() {
	if g.cfg.Bootstrap == nil {
		return
	}
	addrs := g.cfg.Bootstrap()
	if len(addrs) == 0 {
		return
	}
	known := make(map[string]bool)
	for _, m := range g.ml.Members() {
		known[fmt.Sprintf("%s:%d", m.Addr.String(), m.Port)] = true
	}
	var fresh []string
	for _, a := range addrs {
		if !known[a] {
			fresh = append(fresh, a)
		}
	}
	if len(fresh) == 0 {
		return
	}
	n, err := g.ml.Join(fresh)
	if err != nil {
		slog.Debug("gossip: bootstrap join (partial)", "joined", n, "tried", len(fresh), "err", err)
		return
	}
	slog.Info("gossip: joined", "count", n)
}

// Members returns the current memberlist (excluding self) — used
// by `outpost_gossip_edges` to expose the live peer set.
func (g *Gossip) Members() []GossipMember {
	if g == nil || g.ml == nil {
		return nil
	}
	all := g.ml.Members()
	self := g.ml.LocalNode().Name
	out := make([]GossipMember, 0, len(all))
	for _, m := range all {
		if m.Name == self {
			continue
		}
		out = append(out, GossipMember{
			PeerID: PeerID(m.Name),
			Addr:   m.Addr.String(),
			Port:   int(m.Port),
			State:  stateName(m.State),
		})
	}
	return out
}

// GossipMember is the wire shape returned by Members(). One row
// per known member (alive, suspect, or dead) excluding self.
type GossipMember struct {
	PeerID PeerID `json:"peer_id"`
	Addr   string `json:"addr"`
	Port   int    `json:"port"`
	State  string `json:"state"` // alive / suspect / dead / left
}

// gossipDelegate implements memberlist.Delegate — publishes the
// local cache snapshot and merges remote pushes back into the
// cache.
type gossipDelegate struct {
	g *Gossip
}

func (d *gossipDelegate) NodeMeta(limit int) []byte {
	// Pack a short identity hint so peers can render our agent
	// name even before they see a full state push. Truncate to
	// `limit` (memberlist enforces ~512).
	meta := struct {
		AgentName string `json:"a,omitempty"`
		PeerID    PeerID `json:"i,omitempty"`
	}{
		AgentName: d.g.cfg.SelfAgentName,
		PeerID:    d.g.cfg.SelfPeerID,
	}
	b, _ := json.Marshal(meta)
	if len(b) > limit {
		// memberlist will reject oversize meta. Drop the AgentName.
		b, _ = json.Marshal(struct {
			PeerID PeerID `json:"i,omitempty"`
		}{PeerID: d.g.cfg.SelfPeerID})
	}
	return b
}

func (d *gossipDelegate) NotifyMsg(buf []byte) {
	// Direct messages aren't used in Wave 3B.3 — everything goes
	// via local-state/merge-state below.
}

func (d *gossipDelegate) GetBroadcasts(overhead, limit int) [][]byte {
	// No outgoing broadcasts in Wave 3B.3 — cache state is
	// reconciled via LocalState/MergeRemoteState during pull-pushes.
	return nil
}

// gossipLocalState is the structured payload pushed during a SWIM
// pull-push round. The Cache snapshot + a timestamp; remote peers
// merge what they don't already know.
type gossipLocalState struct {
	At    time.Time `json:"at"`
	Peers []Peer    `json:"peers"`
}

func (d *gossipDelegate) LocalState(join bool) []byte {
	snap := d.g.cfg.Cache.Snapshot()
	body := gossipLocalState{
		At:    time.Now(),
		Peers: snap,
	}
	b, _ := json.Marshal(body)
	d.g.mu.Lock()
	d.g.last = body.At
	d.g.mu.Unlock()
	return b
}

func (d *gossipDelegate) MergeRemoteState(buf []byte, join bool) {
	if len(buf) == 0 {
		return
	}
	var body gossipLocalState
	if err := json.Unmarshal(buf, &body); err != nil {
		slog.Debug("gossip: bad remote state", "err", err)
		return
	}
	now := time.Now()
	for _, p := range body.Peers {
		// Skip stale gossip — if the source's observation is
		// older than gossipBroadcastTTL, don't pollute our cache.
		if !p.LastSeenAt.IsZero() && now.Sub(p.LastSeenAt) > gossipBroadcastTTL {
			continue
		}
		p.Sources = append(p.Sources, SourceGossip)
		// Trust level downgrade: we received this from a third
		// party. Even if the source claimed CloudboxCert, we
		// can't verify it without re-probing. Reduce to
		// Unverified — the dial path can re-probe directly
		// later to upgrade.
		p.Trust = TrustUnverified
		d.g.cfg.Cache.Upsert(p)
	}
}

// gossipEventDelegate logs join/leave/failure for now. Wave 3B.4
// will tap it for active/passive promotion (HyParView).
type gossipEventDelegate struct {
	g *Gossip
}

func (e *gossipEventDelegate) NotifyJoin(n *memberlist.Node) {
	slog.Info("gossip: peer joined", "name", n.Name, "addr", n.Addr.String())
}

func (e *gossipEventDelegate) NotifyLeave(n *memberlist.Node) {
	slog.Info("gossip: peer left", "name", n.Name)
}

func (e *gossipEventDelegate) NotifyUpdate(n *memberlist.Node) {
	// Update events are noisy and we already have the data via
	// LocalState merges. Don't log to avoid spam.
}

// stateName maps memberlist's NodeStateType to a short string.
func stateName(s memberlist.NodeStateType) string {
	switch s {
	case memberlist.StateAlive:
		return "alive"
	case memberlist.StateSuspect:
		return "suspect"
	case memberlist.StateDead:
		return "dead"
	case memberlist.StateLeft:
		return "left"
	default:
		return "unknown"
	}
}

// splitBindAddr parses host:port; an empty input gives "0.0.0.0"
// + DefaultGossipBindPort. A port-only input ("=:7946") binds all
// interfaces on the specified port.
func splitBindAddr(s string) (string, int, error) {
	if strings.TrimSpace(s) == "" {
		return "0.0.0.0", DefaultGossipBindPort, nil
	}
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return "", 0, err
	}
	if host == "" {
		host = "0.0.0.0"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}
