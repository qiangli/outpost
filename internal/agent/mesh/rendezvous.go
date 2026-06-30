package mesh

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
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
	}
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
	maddrs := make([]ma.Multiaddr, 0, len(addrs))
	for _, a := range addrs {
		m, err := ma.NewMultiaddr(strings.TrimSpace(a))
		if err != nil {
			continue
		}
		maddrs = append(maddrs, m)
	}
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
