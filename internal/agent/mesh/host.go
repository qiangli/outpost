package mesh

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
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
	Logger  *slog.Logger
}

// Host is the outpost's libp2p peer — the data-plane node of the mesh. It is
// constructed with TCP+QUIC transports, Noise/TLS security, yamux, AutoNAT,
// and DCUtR hole-punching, so it can form direct peer↔peer links across NATs
// and different subnets once a rendezvous (cloudbox) supplies peer addresses.
type Host struct {
	cfg Config
	h   host.Host
	log *slog.Logger
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
		libp2p.EnableNATService(),    // answer AutoNAT dial-backs for peers
		libp2p.EnableHolePunching(),  // DCUtR — direct connect across NATs
		libp2p.NATPortMap(),          // best-effort UPnP/NAT-PMP mapping
		libp2p.UserAgent(ua),
	}
	// Defaults supply TCP+QUIC+WS transports, Noise+TLS security, yamux,
	// and the circuit-relay v2 client that hole-punching coordinates through.

	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("libp2p host: %w", err)
	}
	return &Host{cfg: cfg, h: h, log: log}, nil
}

func listenAddrs(port int) []string {
	return []string{
		fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port),
		fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", port),
		fmt.Sprintf("/ip6/::/tcp/%d", port),
		fmt.Sprintf("/ip6/::/udp/%d/quic-v1", port),
	}
}

// Run logs the host identity + listen addresses and blocks until ctx is
// cancelled, then closes the host. It is the errgroup entry point.
func (m *Host) Run(ctx context.Context) error {
	m.log.Info("mesh: host up",
		"peer_id", m.h.ID().String(),
		"addrs", m.addrStrings(),
	)
	<-ctx.Done()
	m.log.Info("mesh: shutting down", "peer_id", m.h.ID().String())
	return m.h.Close()
}

// Close shuts the host down (for callers not using Run, e.g. tests).
func (m *Host) Close() error { return m.h.Close() }

// PeerID returns this host's stable libp2p peer ID (string form).
func (m *Host) PeerID() string { return m.h.ID().String() }

// LibP2PHost exposes the underlying libp2p host for the forwarder and
// protocol handlers added by later sprint-#8 items.
func (m *Host) LibP2PHost() host.Host { return m.h }

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
	PeerID         string   `json:"peer_id"`
	ListenAddrs    []string `json:"listen_addrs"`
	ConnectedPeers int      `json:"connected_peers"`
}

// Status returns a live snapshot of the mesh host.
func (m *Host) Status() Status {
	return Status{
		PeerID:         m.h.ID().String(),
		ListenAddrs:    m.addrStrings(),
		ConnectedPeers: len(m.h.Network().Peers()),
	}
}
