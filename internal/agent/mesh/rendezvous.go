package mesh

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	ma "github.com/multiformats/go-multiaddr"

	"github.com/qiangli/outpost/internal/agent/peerplane"
	"github.com/qiangli/outpost/internal/agent/peerstatus"
)

// Rendezvous wires the mesh Host to cloudbox's peer-signal surface — the SOLE
// rendezvous for the fabric (no third-party discovery; every outpost already
// holds a tunnel to cloudbox). Each tick it announces this host's peer id +
// dialable multiaddrs, discovers the paired-host list, and dials each peer.
// libp2p upgrades the link to a direct hole-punched one (DCUtR) where possible;
// cloudbox only brokers the introduction — the bytes then go peer-to-peer.
// See docs/libp2p-mesh-transport.md.
type Rendezvous struct {
	host        *Host
	agentName   string
	cloudboxURL string
	accessToken string
	hc          *http.Client
	signal      *peerplane.Client
	log         *slog.Logger
	interval    time.Duration

	// hostPeers maps a paired host name → its libp2p peer id, learned each
	// tick from the connect/dial loop (the same cloudbox host↔peer-id
	// resolution shard discovery uses). It's the bridge that lets a
	// hostname-keyed caller (peer-status rows) ask the mesh — which knows
	// peers only by id — for the live link class to that host.
	mu        sync.Mutex
	hostPeers map[string]string
}

// NewRendezvous builds the rendezvous client for a mesh host. cloudboxURL +
// accessToken are the paired outpost's existing cloudbox credentials (the same
// the peerplane + registry push use).
func NewRendezvous(host *Host, agentName, cloudboxURL, accessToken string, log *slog.Logger) *Rendezvous {
	if log == nil {
		log = slog.Default()
	}
	hc := &http.Client{Timeout: 15 * time.Second}
	return &Rendezvous{
		host:        host,
		agentName:   agentName,
		cloudboxURL: cloudboxURL,
		accessToken: accessToken,
		hc:          hc,
		signal:      &peerplane.Client{BaseURL: cloudboxURL, Token: accessToken, HC: hc},
		log:         log,
		interval:    60 * time.Second,
		hostPeers:   make(map[string]string),
	}
}

// LinkClassForHost returns the mesh link class ("tp"/"lan"/"wan"/"") of the
// DIRECT connection to the named paired host. Back-compat shim over
// LinkInfoForHost (which also carries the LAN label).
func (r *Rendezvous) LinkClassForHost(host string) string {
	return r.LinkInfoForHost(host).Class
}

// LinkInfoForHost returns the mesh link class AND the LAN label (which local
// LAN the direct link rides over) for the named paired host, or a zero
// LinkInfo when the host's peer id isn't known yet or there's no direct link.
// The class+label are computed live from the current connection state — the
// host→peer-id map is only the lookup key. This is the accurate same-LAN signal
// (enriched with WHICH lan) that peer-status overlays on cloudbox's egress-IP
// heuristic.
func (r *Rendezvous) LinkInfoForHost(host string) LinkInfo {
	if r == nil {
		return LinkInfo{}
	}
	r.mu.Lock()
	peerID := r.hostPeers[host]
	r.mu.Unlock()
	if peerID == "" {
		return LinkInfo{}
	}
	return r.host.PeerLinkInfo(peerID)
}

// rememberPeer records the host→peer-id association learned during a tick.
func (r *Rendezvous) rememberPeer(host, peerID string) {
	if host == "" || peerID == "" {
		return
	}
	r.mu.Lock()
	r.hostPeers[host] = peerID
	r.mu.Unlock()
}

// Run announces + discovers on a timer until ctx is cancelled.
func (r *Rendezvous) Run(ctx context.Context) error {
	// Settle so the host has computed its reachable addresses (interface
	// expansion + the first AutoNAT/identify observations).
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
	}
	r.tick(ctx)
	// Active reconnect sweep — a faster (~30s, jittered) force-dialing recovery
	// loop for owned/shared peers libp2p reports disconnected. Complements the
	// slower announce/discover tick and mDNS: it re-resolves fresh candidate
	// addrs and force-dials, so a same-LAN pair that dropped after a restart
	// reconnects without operator action. Joined before Run returns.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); r.reconnectSweep(ctx) }()
	defer wg.Wait()
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			r.tick(ctx)
		}
	}
}

// Reconnect-sweep cadence: faster than the 60s announce/discover tick so a
// dropped link recovers quickly, jittered so a fleet-wide restart doesn't
// synchronize every host's sweep onto the same instant.
const (
	sweepInterval = 30 * time.Second
	sweepJitter   = 10 * time.Second
)

// reconnectSweep runs sweepOnce on a jittered ~30s cadence until ctx cancels.
func (r *Rendezvous) reconnectSweep(ctx context.Context) {
	for {
		wait := sweepInterval + rand.N(sweepJitter)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		r.sweepOnce(ctx)
	}
}

// redialTarget is one owned/shared peer the sweep should attempt to reconnect.
type redialTarget struct {
	Host   string
	PeerID string
}

// peersToRedial selects, from the known host→peer-id map, the peers that are
// NOT currently connected — the redial candidates. Pure over the connectedness
// lookup (so it is unit-testable without a live swarm) and deterministically
// ordered by host so a sweep is reproducible.
func peersToRedial(hostPeers map[string]string, connected func(peerID string) bool) []redialTarget {
	out := make([]redialTarget, 0, len(hostPeers))
	for host, pid := range hostPeers {
		if host == "" || pid == "" {
			continue
		}
		if connected(pid) {
			continue
		}
		out = append(out, redialTarget{Host: host, PeerID: pid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// connectedByID reports whether libp2p currently holds any connection to the
// peer id (an undecodable id is treated as connected, i.e. skipped).
func (r *Rendezvous) connectedByID(peerID string) bool {
	pid, err := peer.Decode(peerID)
	if err != nil {
		return true
	}
	return r.host.LibP2PHost().Network().Connectedness(pid) == network.Connected
}

// snapshotHostPeers copies the host→peer-id map under the lock.
func (r *Rendezvous) snapshotHostPeers() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.hostPeers))
	for h, p := range r.hostPeers {
		out[h] = p
	}
	return out
}

// sweepOnce redials every known owned/shared peer that is currently
// disconnected. The peer set is what the announce/discover tick has learned
// (r.hostPeers); the tick keeps discovering new peers, the sweep keeps the
// known ones connected. One sweep at a time (Run calls it serially); every
// error just logs and continues — a sweep must never disturb healthy links.
func (r *Rendezvous) sweepOnce(ctx context.Context) {
	snapshot := r.snapshotHostPeers()
	if len(snapshot) == 0 {
		return
	}
	for _, t := range peersToRedial(snapshot, r.connectedByID) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		r.redialPeer(ctx, t)
	}
}

// redialPeer re-resolves fresh candidate addrs for one disconnected peer via
// the rendezvous (cloudbox), merges them with any peerstore/mDNS-cached addrs,
// clears dial backoff, and force-dials directly. Best-effort throughout — a
// failed resolve still tries cached addrs; a failed dial just logs.
func (r *Rendezvous) redialPeer(ctx context.Context, t redialTarget) {
	pid, err := peer.Decode(t.PeerID)
	if err != nil {
		return
	}
	h := r.host.LibP2PHost()
	if pid == h.ID() {
		return
	}
	// Re-resolve fresh candidates from cloudbox (best-effort). This is the same
	// resolve the discover tick uses; here it feeds the peerstore before a
	// force-dial rather than a plain Connect.
	if tgt, rerr := r.signal.Connect(ctx, r.agentName, t.Host); rerr == nil {
		if tgt.Peer.PeerID != "" {
			r.rememberPeer(t.Host, tgt.Peer.PeerID)
		}
		if fresh := parseMultiaddrs(tgt.Peer.Candidates); len(fresh) > 0 {
			h.Peerstore().AddAddrs(pid, fresh, peerstore.TempAddrTTL)
		}
	} else {
		r.log.Debug("mesh sweep: resolve failed", "peer", t.Host, "err", rerr)
	}
	// Union of fresh + previously-cached (peerstore/mDNS) addrs.
	addrs := h.Peerstore().Addrs(pid)
	if len(addrs) == 0 {
		r.log.Debug("mesh sweep: no candidate addrs", "peer", t.Host)
		return
	}
	if sw, ok := h.Network().(*swarm.Swarm); ok {
		sw.Backoff().Clear(pid)
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	dctx = network.WithForceDirectDial(dctx, "mesh-reconnect-sweep")
	if err := h.Connect(dctx, peer.AddrInfo{ID: pid, Addrs: addrs}); err != nil {
		r.log.Debug("mesh sweep: redial failed", "peer", t.Host, "err", err)
		return
	}
	r.log.Info("mesh sweep: reconnected peer", "peer", t.Host, "direct", r.host.HasDirectConn(t.PeerID))
}

func (r *Rendezvous) tick(ctx context.Context) {
	r.announce(ctx)
	r.discoverAndDial(ctx)
	r.drainInbox(ctx)
}

// announce publishes this host's peer id + dialable multiaddrs + exposed mesh
// service names (the service registry) to cloudbox.
func (r *Rendezvous) announce(ctx context.Context) {
	addrs := r.host.dialableAddrs()
	if len(addrs) == 0 {
		return
	}
	// Advertise the forwarder's exposed service names so peers can resolve
	// them by name. Non-nil (even empty) so the registry tracks the current set.
	exposed := r.host.Forwarder().Snapshot().Exposed
	services := make([]string, 0, len(exposed))
	for name := range exposed {
		services = append(services, name)
	}
	sort.Strings(services)
	if err := r.signal.Announce(ctx, r.agentName, r.host.PeerID(), addrs, services); err != nil {
		r.log.Debug("mesh: announce failed", "err", err)
	}
}

// discoverAndDial fetches the paired-host list and dials each online peer via a
// Connect-signal (which also enqueues a reciprocal notice for the peer).
func (r *Rendezvous) discoverAndDial(ctx context.Context) {
	peers, err := peerstatus.Fetch(ctx, r.cloudboxURL, r.accessToken, r.hc)
	if err != nil {
		r.log.Debug("mesh: peer list fetch failed", "err", err)
		return
	}
	for _, p := range peers {
		if p.Host == r.agentName || !p.Online {
			continue
		}
		tgt, err := r.signal.Connect(ctx, r.agentName, p.Host)
		if err != nil {
			r.log.Debug("mesh: connect-signal failed", "peer", p.Host, "err", err)
			continue
		}
		r.dial(ctx, tgt.Peer.PeerID, tgt.Peer.Candidates, p.Host)
	}
}

// drainInbox dials back peers that requested a rendezvous — the source side of
// a DCUtR hole-punch, so both ends dial simultaneously.
func (r *Rendezvous) drainInbox(ctx context.Context) {
	box, err := r.signal.Inbox(ctx, r.agentName)
	if err != nil {
		r.log.Debug("mesh: inbox failed", "err", err)
		return
	}
	for _, n := range box {
		r.dial(ctx, n.FromPeerID, n.FromCandidates, n.FromHost)
	}
}

// dial adds the peer's addrs to the peerstore and opens a connection; libp2p
// upgrades to a direct hole-punched link when the path allows.
func (r *Rendezvous) dial(ctx context.Context, peerID string, addrs []string, label string) {
	if peerID == "" || len(addrs) == 0 {
		return
	}
	pid, err := peer.Decode(peerID)
	if err != nil {
		r.log.Debug("mesh: bad peer id", "peer", label, "err", err)
		return
	}
	h := r.host.LibP2PHost()
	if pid == h.ID() {
		return // ourselves
	}
	// Record host→peer-id (regardless of dial outcome) so peer-status can ask
	// for this host's live link class even when the link is currently relayed.
	r.rememberPeer(label, peerID)
	maddrs := parseMultiaddrs(addrs)
	if len(maddrs) == 0 {
		return
	}
	h.Peerstore().AddAddrs(pid, maddrs, peerstore.TempAddrTTL)
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// Plain Connect is a no-op when ANY connection exists — including a relayed
	// one. If we're connected ONLY via the cloudbox relay (no direct link), force
	// a direct dial to upgrade: otherwise a same-LAN peer stays stranded on the
	// slow relay and shard discovery (which requires a direct lan/tp link) drops
	// it. ForceDirectDial makes the swarm dial the peer's direct addrs, skipping
	// relay; once a direct link exists HasDirectConn is true and this path is
	// skipped, and a genuinely-remote peer simply keeps failing the upgrade and
	// stays on the relay — so nothing healthy is disturbed.
	if h.Network().Connectedness(pid) == network.Connected && !r.host.HasDirectConn(peerID) {
		dctx = network.WithForceDirectDial(dctx, "mesh-direct-upgrade")
	}
	if err := h.Connect(dctx, peer.AddrInfo{ID: pid, Addrs: maddrs}); err != nil {
		r.log.Debug("mesh: dial failed", "peer", label, "err", err)
		return
	}
	r.log.Info("mesh: connected to peer", "peer", label, "peer_id", peerID, "direct", r.host.HasDirectConn(peerID))
}

// parseMultiaddrs turns candidate multiaddr strings into multiaddrs, dropping
// malformed entries. Shared by the discover dial and the reconnect sweep.
func parseMultiaddrs(addrs []string) []ma.Multiaddr {
	out := make([]ma.Multiaddr, 0, len(addrs))
	for _, a := range addrs {
		m, err := ma.NewMultiaddr(strings.TrimSpace(a))
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}
