package mesh

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// Config configures the mesh host.
type Config struct {
	// AgentName is this outpost's name, surfaced in the libp2p user-agent.
	AgentName string
	// ListenPort is the TCP+QUIC listen port; 0 = an ephemeral port per
	// transport. A stable port helps NAT/hole-punch and the loopback
	// forwarder added by later sprint-#8 items.
	ListenPort int
	// PrivKey, when non-nil, is used as the host identity instead of the
	// persistent on-disk key. Tests pass an ephemeral key; production
	// leaves it nil so LoadOrCreateKey owns the stable peer ID.
	PrivKey crypto.PrivKey
	// RelayAddrs are circuit-relay v2 relay multiaddrs (cloudbox's relay,
	// each ending in /p2p/<relay-id>). When set, the host runs AutoRelay
	// against them — it reserves a slot, advertises a relayed address, and
	// DCUtR upgrades the relayed link to a direct hole-punched one. This is
	// what lets two strict-NAT peers connect when neither is directly
	// reachable (same-LAN/same-vicinity needs no relay).
	RelayAddrs []string
	Logger     *slog.Logger
	// DisableMDNS turns off local-LAN mDNS peer discovery. Production leaves it
	// false (mDNS on — same-LAN peers connect directly). Tests set it true so
	// real multicast can't discover sibling test hosts (or real outposts on the
	// LAN) and perturb exact connected-peer-count assertions.
	DisableMDNS bool
}

// Host is the outpost's libp2p peer — the data-plane node of the mesh. It is
// constructed with TCP+QUIC transports, Noise/TLS security, yamux, AutoNAT,
// and DCUtR hole-punching, so it can form direct peer↔peer links across NATs
// and different subnets once a rendezvous (cloudbox) supplies peer addresses.
type Host struct {
	cfg Config
	h   host.Host
	log *slog.Logger
	fwd *Forwarder

	// mDNS lifecycle. newMDNS builds a fresh (started) LAN-discovery service;
	// it is nil when mDNS is disabled (DisableMDNS / tests). mdns is the active
	// service, guarded by mdnsMu — the supervisor (superviseMDNS) swaps it when
	// it (re)starts, and Close/Run tear it down. A nil newMDNS makes the
	// supervisor a no-op.
	mdnsMu  sync.Mutex
	mdns    mdnsService
	newMDNS func() (mdnsService, error)
	// mdnsRefresh overrides the proactive-refresh cadence; 0 = mdnsRefreshInterval.
	// Only tests set it (to exercise the supervisor without minute-long waits).
	mdnsRefresh time.Duration
}

// New builds the libp2p host with the persistent (or supplied) mesh identity.
func New(cfg Config) (*Host, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	priv := cfg.PrivKey
	if priv == nil {
		var err error
		priv, err = LoadOrCreateKey()
		if err != nil {
			return nil, fmt.Errorf("mesh identity: %w", err)
		}
	}

	ua := "outpost-mesh"
	if cfg.AgentName != "" {
		ua += "/" + cfg.AgentName
	}

	opts := []libp2p.Option{
		libp2p.Identity(priv),
		libp2p.ListenAddrStrings(listenAddrs(cfg.ListenPort)...),
		libp2p.EnableNATService(),   // answer AutoNAT dial-backs for peers
		libp2p.EnableHolePunching(), // DCUtR — direct connect across NATs
		libp2p.NATPortMap(),         // best-effort UPnP/NAT-PMP mapping
		libp2p.UserAgent(ua),
	}
	// Defaults supply TCP+QUIC+WS transports, Noise+TLS security, yamux,
	// and the circuit-relay v2 client that hole-punching coordinates through.

	// When a relay (cloudbox) is configured, run AutoRelay against it so a
	// strict-NAT host reserves a slot + advertises a relayed addr that DCUtR
	// can then upgrade to a direct link.
	if relays := parseRelays(cfg.RelayAddrs, log); len(relays) > 0 {
		opts = append(opts, libp2p.EnableAutoRelayWithStaticRelays(relays))
		log.Info("mesh: AutoRelay enabled", "relays", len(relays))
	}

	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("libp2p host: %w", err)
	}
	m := &Host{cfg: cfg, h: h, log: log}
	m.fwd = newForwarder(m, log) // registers the forward stream handler

	// Local-LAN mDNS discovery: advertise our on-link addresses and dial
	// same-LAN siblings DIRECTLY, instead of hole-punching over the public IP
	// the cloudbox rendezvous hands over. Non-fatal — a multicast/socket
	// failure (locked-down network, no multicast) degrades to the existing
	// relay/rendezvous path; libp2p still prefers a direct link and falls back
	// to the relay on its own.
	//
	// The factory (not a one-shot start) is what lets superviseMDNS restart the
	// service if libp2p's mDNS goroutines die/go-silent after a network blip or
	// a daemon restart — the reliability bug this addresses. We do an initial
	// start here so same-LAN discovery works immediately; the supervisor
	// (started from Run) keeps it alive thereafter.
	if !cfg.DisableMDNS {
		m.newMDNS = func() (mdnsService, error) { return startMDNS(h, log) }
		if err := m.restartMDNS(); err != nil {
			log.Warn("mesh: mDNS LAN discovery unavailable at start; supervisor will retry", "err", err)
		}
	}

	return m, nil
}

// restartMDNS (re)builds the LAN-discovery service under mdnsMu: it closes any
// running instance, constructs+starts a fresh one via newMDNS, and stores it.
// On failure it leaves m.mdns == nil so the supervisor knows to retry sooner.
// A nil newMDNS (mDNS disabled) is a no-op.
func (m *Host) restartMDNS() error {
	m.mdnsMu.Lock()
	defer m.mdnsMu.Unlock()
	if m.newMDNS == nil {
		return nil
	}
	if m.mdns != nil {
		_ = m.mdns.Close()
		m.mdns = nil
	}
	svc, err := m.newMDNS()
	if err != nil {
		return err
	}
	m.mdns = svc
	return nil
}

// mdnsHealthy reports whether an mDNS service is currently active (nil-safe).
func (m *Host) mdnsHealthy() bool {
	m.mdnsMu.Lock()
	defer m.mdnsMu.Unlock()
	return m.mdns != nil
}

// closeMDNS tears down the active mDNS service (nil-safe) and disables further
// (re)starts, so Close/Run shutdown is clean.
func (m *Host) closeMDNS() {
	m.mdnsMu.Lock()
	defer m.mdnsMu.Unlock()
	m.newMDNS = nil
	if m.mdns != nil {
		_ = m.mdns.Close()
		m.mdns = nil
	}
}

// mDNS supervisor cadence. libp2p's mDNS service exposes no health signal, so
// the supervisor proactively rebuilds it on a slow cadence (recovering a
// silently-dead multicast listener within one interval) and retries fast, with
// exponential backoff, whenever a (re)start fails.
const (
	mdnsRefreshInterval = 2 * time.Minute
	mdnsRetryBackoffMin = 5 * time.Second
	mdnsRetryBackoffMax = 2 * time.Minute
)

// superviseMDNS keeps LAN discovery alive for the life of ctx. On a healthy
// service it refreshes every mdnsRefreshInterval (the only way to recover a
// silently-dead libp2p mDNS, which has no error channel); after a failed
// (re)start it retries on an exponential backoff. It is a no-op when mDNS is
// disabled. Started from Run; returns on ctx cancellation.
func (m *Host) superviseMDNS(ctx context.Context) {
	m.mdnsMu.Lock()
	enabled := m.newMDNS != nil
	m.mdnsMu.Unlock()
	if !enabled {
		return
	}
	refresh := m.mdnsRefresh
	if refresh <= 0 {
		refresh = mdnsRefreshInterval
	}
	// backoff/failing govern the retry cadence ONLY after a failed (re)start;
	// on the healthy path we proactively rebuild every refresh interval — the
	// sole way to recover a libp2p mDNS service that died silently (no error
	// channel), which is what the reconnect bug exposed.
	backoff := mdnsRetryBackoffMin
	failing := false
	for {
		wait := refresh
		if failing {
			wait = backoff
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if m.newMDNS == nil { // closed concurrently
			return
		}
		if err := m.restartMDNS(); err != nil {
			failing = true
			backoff = min(backoff*2, mdnsRetryBackoffMax)
			m.log.Warn("mesh mdns: (re)start failed; will retry", "err", err, "retry_in", backoff)
			continue
		}
		failing = false
		backoff = mdnsRetryBackoffMin
		m.log.Info("mesh mdns: LAN discovery (re)started")
	}
}

func listenAddrs(port int) []string {
	return []string{
		fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port),
		fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", port),
		fmt.Sprintf("/ip6/::/tcp/%d", port),
		fmt.Sprintf("/ip6/::/udp/%d/quic-v1", port),
	}
}

// parseRelays turns relay multiaddr strings (each ending in /p2p/<id>) into
// AddrInfos, skipping malformed entries with a warning.
func parseRelays(addrs []string, log *slog.Logger) []peer.AddrInfo {
	var infos []peer.AddrInfo
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		m, err := ma.NewMultiaddr(a)
		if err != nil {
			log.Warn("mesh: bad relay multiaddr", "addr", a, "err", err)
			continue
		}
		info, err := peer.AddrInfoFromP2pAddr(m)
		if err != nil {
			log.Warn("mesh: relay multiaddr missing /p2p/<id>", "addr", a, "err", err)
			continue
		}
		infos = append(infos, *info)
	}
	return infos
}

// Run logs the host identity + listen addresses and blocks until ctx is
// cancelled, then closes the host. It is the errgroup entry point.
func (m *Host) Run(ctx context.Context) error {
	m.log.Info("mesh: host up",
		"peer_id", m.h.ID().String(),
		"addrs", m.addrStrings(),
	)
	// Keep LAN mDNS discovery alive (restart it if it dies/goes silent) for the
	// life of the host. Returns on ctx cancellation.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); m.superviseMDNS(ctx) }()
	<-ctx.Done()
	wg.Wait()
	m.log.Info("mesh: shutting down", "peer_id", m.h.ID().String())
	m.closeMDNS()
	return m.h.Close()
}

// Close shuts the host down (for callers not using Run, e.g. tests).
func (m *Host) Close() error {
	m.closeMDNS()
	return m.h.Close()
}

// PeerID returns this host's stable libp2p peer ID (string form).
func (m *Host) PeerID() string { return m.h.ID().String() }

// LibP2PHost exposes the underlying libp2p host for protocol handlers added by
// later sprint-#8 items.
func (m *Host) LibP2PHost() host.Host { return m.h }

// Forwarder is the loopback-TCP-over-mesh transport bound to this host (the
// stream handler is registered at construction). Expose local services on the
// worker side; Listen for a (peer, service) on the client side.
func (m *Host) Forwarder() *Forwarder { return m.fwd }

// HasDirectConn reports whether there is a DIRECT (non-relayed) connection to the
// peer — the mesh-native "local / same-vicinity" signal: a relayed connection
// (network.Limited) means the peer is reachable only over the WAN relay, i.e.
// remote. The mobility-aware mirror's lan_only gate uses this to mirror only while
// the pair is genuinely local (and pause when it falls back to relay).
func (m *Host) HasDirectConn(peerID string) bool {
	pid, err := peer.Decode(peerID)
	if err != nil {
		return false
	}
	for _, c := range m.h.Network().ConnsToPeer(pid) {
		if !c.Stat().Limited {
			return true
		}
	}
	return false
}

// LinkInfo is the per-peer direct-link summary the peer-status overlay needs:
// the strongest direct link Class (tp/lan/wan; "" when relayed/absent) PLUS a
// LAN label naming WHICH local LAN that strongest link rides over. The class
// collapses every private network into "lan", so a node on Wi-Fi *and* a wired
// crosslink can't tell its peers apart by class alone — the LAN label (derived
// from the winning connection's LOCAL multiaddr) is what disambiguates them.
type LinkInfo struct {
	Class string // strongest direct-link class: tp>lan>wan; "" if relayed/none
	LAN   string // local LAN label of the winning conn (see localLANLabel); "" if none
}

// PeerLinkClass classifies a DIRECT (non-relayed) connection to the peer by its
// remote address — the ground truth for same-locality that the peerplane's UDP
// probes miss (they can't dial a zone-less link-local address, and a firewalled
// LAN drops the echo, so genuinely-local peers come back "unreached"):
//
//	"tp"  — link-local / APIPA (169.254.x, fe80:) : a dedicated point-to-point wired link
//	"lan" — RFC-1918 / ULA private address          : same LAN (incl. wifi)
//	"wan" — public address                          : remote
//	""    — no direct connection (relayed or absent)
//
// It returns the strongest class across all direct connections to the peer.
// Kept for back-compat; implemented via PeerLinkInfo.
func (m *Host) PeerLinkClass(peerID string) string {
	return m.PeerLinkInfo(peerID).Class
}

// PeerLinkInfo returns the strongest direct-link class to the peer AND the LAN
// label of the local interface that strongest link uses. It walks every direct
// (non-relayed) connection, keeps the one whose REMOTE-addr class wins
// (tp>lan>wan), and derives LinkInfo.LAN from THAT connection's LOCAL multiaddr
// — because the class alone collapses all private LANs into "lan", but the
// local subnet/interface identifies which one (Wi-Fi vs. a wired crosslink vs.
// a second LAN).
func (m *Host) PeerLinkInfo(peerID string) LinkInfo {
	pid, err := peer.Decode(peerID)
	if err != nil {
		return LinkInfo{}
	}
	info := LinkInfo{}
	for _, c := range m.h.Network().ConnsToPeer(pid) {
		if c.Stat().Limited {
			continue // relayed — not a direct link
		}
		cls := classifyConnAddr(c.RemoteMultiaddr())
		if cls == "" {
			continue // loopback / unclassifiable — no locality signal
		}
		// Take the LAN label from the connection whose class STRICTLY wins, so
		// it always reflects the strongest link the peer is reached over.
		if strongerLinkClass(info.Class, cls) == cls && cls != info.Class {
			info.Class = cls
			info.LAN = localLANLabel(c.LocalMultiaddr())
		}
	}
	return info
}

// localLANLabel derives a short label naming the local LAN/path a link uses from
// the connection's LOCAL multiaddr — the only place the specific LAN (not just
// "private") is observable. The class can't tell two private LANs apart; this
// can:
//
//	link-local (IPv4 169.254.0.0/16, IPv6 fe80::/10) → "wired" (direct point-to-point crosslink)
//	private    (RFC-1918 10/8, 172.16/12, 192.168/16) → subnet base, e.g. "10.0.0" (first three octets)
//	private    (ULA fc00::/7)                          → "ula"
//	public / loopback / unparseable                    → "" (no LAN label)
func localLANLabel(localAddr ma.Multiaddr) string {
	if localAddr == nil {
		return ""
	}
	ipStr, err := localAddr.ValueForProtocol(ma.P_IP4)
	isV4 := err == nil
	if !isV4 {
		if ipStr, err = localAddr.ValueForProtocol(ma.P_IP6); err != nil {
			return ""
		}
	}
	ip := net.ParseIP(ipStr)
	if ip == nil || ip.IsLoopback() {
		return ""
	}
	if ip.IsLinkLocalUnicast() {
		return "wired" // 169.254.x / fe80: — a dedicated point-to-point wired link
	}
	if !ip.IsPrivate() {
		return "" // public address — no LAN label
	}
	if isV4 {
		if v4 := ip.To4(); v4 != nil {
			return fmt.Sprintf("%d.%d.%d", v4[0], v4[1], v4[2]) // /24 base
		}
	}
	return "ula" // ULA fc00::/7 — short prefix label
}

func classifyConnAddr(maddr ma.Multiaddr) string {
	if maddr == nil {
		return ""
	}
	ipStr, err := maddr.ValueForProtocol(ma.P_IP4)
	if err != nil {
		ipStr, _ = maddr.ValueForProtocol(ma.P_IP6)
	}
	ip := net.ParseIP(ipStr)
	if ip == nil || ip.IsLoopback() {
		return ""
	}
	switch {
	case ip.IsLinkLocalUnicast():
		return "tp"
	case ip.IsPrivate():
		return "lan"
	default:
		return "wan"
	}
}

func strongerLinkClass(a, b string) string {
	rank := map[string]int{"": 0, "wan": 1, "lan": 2, "tp": 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// Connected reports whether there is any connection (direct or relayed) to peer.
func (m *Host) Connected(peerID string) bool {
	pid, err := peer.Decode(peerID)
	if err != nil {
		return false
	}
	return len(m.h.Network().ConnsToPeer(pid)) > 0
}

// dialableAddrs returns the host's reachable multiaddrs as strings, dropping
// unspecified (0.0.0.0 / ::) listen addrs that no remote peer can dial. These
// are what we announce to cloudbox for peers to dial back. libp2p's host.Addrs()
// already expands a 0.0.0.0 listen to the concrete interface addresses; this
// just filters any residual wildcard.
func (m *Host) dialableAddrs() []string {
	addrs := m.h.Addrs()
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		s := a.String()
		if strings.Contains(s, "/0.0.0.0/") || strings.Contains(s, "/::/") {
			continue
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func (m *Host) addrStrings() []string {
	addrs := m.h.Addrs()
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	sort.Strings(out)
	return out
}

// Status is a snapshot for admincore / status surfaces.
type Status struct {
	PeerID         string     `json:"peer_id"`
	ListenAddrs    []string   `json:"listen_addrs"`
	ConnectedPeers int        `json:"connected_peers"`
	Peers          []PeerConn `json:"peers,omitempty"`
}

// PeerConn is the per-connected-peer link detail for the local mesh-status
// debug surface: which remote address(es) the peer is reached over and the
// strongest link class across its direct connections. This is a LOCAL
// loopback/admin view (the owner inspecting their own daemon) — raw remote
// addrs are fine here and deliberately NOT surfaced by the cross-account
// peer-status API.
type PeerConn struct {
	ID        string   `json:"id"`         // peer id (string form)
	Direct    bool     `json:"direct"`     // at least one non-relayed connection
	LinkClass string   `json:"link_class"` // strongest of its direct conns: tp>lan>wan; "" if relayed/none
	Remote    []string `json:"remote"`     // remote multiaddr string(s)
}

// Status returns a live snapshot of the mesh host.
func (m *Host) Status() Status {
	return Status{
		PeerID:         m.h.ID().String(),
		ListenAddrs:    m.addrStrings(),
		ConnectedPeers: len(m.h.Network().Peers()),
		Peers:          m.peerConns(),
	}
}

// peerConns builds the per-connected-peer link detail by walking every
// connection to each connected peer: it records the remote multiaddrs, marks
// the peer Direct when any connection is non-relayed, and computes LinkClass as
// the strongest class across its DIRECT connections (relayed conns don't count
// toward locality). Reuses the same classifyConnAddr/strongerLinkClass helpers
// PeerLinkClass uses.
func (m *Host) peerConns() []PeerConn {
	peers := m.h.Network().Peers()
	out := make([]PeerConn, 0, len(peers))
	for _, pid := range peers {
		conns := m.h.Network().ConnsToPeer(pid)
		if len(conns) == 0 {
			continue
		}
		pc := PeerConn{ID: pid.String()}
		for _, c := range conns {
			raddr := c.RemoteMultiaddr()
			pc.Remote = append(pc.Remote, raddr.String())
			if isRelayed(c) {
				continue // relayed — not a direct link, no class
			}
			pc.Direct = true
			pc.LinkClass = strongerLinkClass(pc.LinkClass, classifyConnAddr(raddr))
		}
		out = append(out, pc)
	}
	return out
}

// isRelayed reports whether a connection rides the circuit relay (i.e. it is
// not a direct peer-to-peer link). A relayed conn is marked Limited by libp2p
// and its remote multiaddr carries the /p2p-circuit component.
func isRelayed(c connStat) bool {
	if c.Stat().Limited {
		return true
	}
	if a := c.RemoteMultiaddr(); a != nil && strings.Contains(a.String(), "/p2p-circuit") {
		return true
	}
	return false
}

// connStat is the slice of network.Conn that isRelayed needs — kept narrow so
// the relay check is unit-testable without a full libp2p connection.
type connStat interface {
	Stat() network.ConnStats
	RemoteMultiaddr() ma.Multiaddr
}
