// Roadmap item #17 — gossip layer for the discovery cache.
//
// Without gossip, each outpost only sees the edges IT dialed
// directly. The reachability ledger surfaces those local edges,
// but `outpost peers route-to` is severely limited: a peer that
// every fleet member CAN reach but THIS box can't dial directly
// is invisible.
//
// With gossip, the fleet shares its edge views. Every outpost
// exchanges (and merges) a compact summary of (a) which peers it
// knows about and (b) the freshness of its most-recent observation
// of each. The discovery Cache absorbs gossiped peers with
// SourceGossip, so route-to can use second-hand reachability
// to suggest paths.
//
// Implementation: clean-room push-pull anti-entropy over plain
// TCP (stdlib only). One member per outpost on the gossip port;
// each connection is a single request/response state exchange —
// the dialer sends its cache snapshot, the listener merges it and
// replies with its own. Bootstrap dial targets come from mDNS and
// the cloudbox NAT-locality hints we already collect; liveness is
// age-derived from the most recent successful exchange. This
// replaces the former hashicorp/memberlist SWIM transport (MPL
// dependency) without changing the merge semantics: gossiped
// peers enter the cache with SourceGossip and TrustUnverified.

package discovery

import (
	"bufio"
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
)

// DefaultGossipBindPort is the TCP port the gossip listener binds.
// Operator can override via FileConfig.GossipBindAddr (host:port).
// 7946 is the recognized "SWIM" port — kept deliberately so
// existing firewall-allow rules for the old transport still apply.
const DefaultGossipBindPort = 7946

// gossipBroadcastTTL is how long a gossiped Peer should remain in
// the cache before being treated as stale. Longer than the
// 5-min observation tick (so gossip survives a tick miss); shorter
// than the Cache TTL (so non-refreshed gossip-only peers age out).
const gossipBroadcastTTL = 10 * time.Minute

// Member-state age thresholds. A member is "alive" while exchanges
// keep succeeding, "suspect" once they stop for a while, "dead"
// after that. Entries older than gossipDeadAfter are pruned.
const (
	gossipAliveAfter   = 90 * time.Second
	gossipSuspectAfter = 10 * time.Minute
	gossipDeadAfter    = 30 * time.Minute
)

// gossipExchangeTimeout bounds a single dial/read/write, and
// maxGossipStateBytes bounds the state blob we'll accept.
const (
	gossipExchangeTimeout = 10 * time.Second
	maxGossipStateBytes   = 8 << 20 // 8 MiB
)

// GossipConfig is the input to NewGossip.
type GossipConfig struct {
	// SelfPeerID identifies this outpost on the gossip mesh.
	SelfPeerID PeerID

	// SelfAgentName is the human-friendly name (PeerID is opaque).
	SelfAgentName string

	// BindAddr is the local listen address (host:port). Empty
	// uses 0.0.0.0:DefaultGossipBindPort. Port 0 picks an
	// ephemeral port (tests); the bound address is available
	// via (*Gossip).Addr once Run has bound it.
	BindAddr string

	// AdvertiseAddr is the address we claim as the source of our
	// state exchanges. Defaults to the bound listener address.
	// Override when a NAT rewrites the source IP.
	AdvertiseAddr string

	// Cache is the live discovery cache. Gossip-received peers
	// get Upserted here with SourceGossip.
	Cache *Cache

	// Bootstrap is a callback returning a list of dial addresses
	// (host:port). Called at boot AND on the periodic push-pull
	// ticker so newly-discovered peers (via mDNS or NAT hints)
	// get pulled into the mesh. May return an empty list — gossip
	// still works as a single-node sink that accepts pushes.
	Bootstrap func() []string

	// PushPullInterval is how often the bootstrap addresses are
	// re-dialed for a state exchange. Zero uses 60 seconds.
	PushPullInterval time.Duration

	// Logger is optional; nil = slog-default. When set, gossip
	// log lines are written there instead.
	Logger io.Writer
}

// Gossip is the lifecycle handle. Construct via NewGossip; call
// Run to start the listener + push-pull loop; closes on ctx.
type Gossip struct {
	cfg GossipConfig

	mu      sync.RWMutex
	ln      net.Listener
	members map[PeerID]*gossipMemberState
	last    time.Time // last successful outbound state push

	log *slog.Logger
}

// gossipMemberState is the per-peer liveness record behind
// Members().
type gossipMemberState struct {
	AgentName string
	Addr      string
	Port      int
	LastOK    time.Time
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
	log := slog.Default()
	if cfg.Logger != nil {
		log = slog.New(slog.NewTextHandler(cfg.Logger, nil))
	}
	return &Gossip{
		cfg:     cfg,
		members: make(map[PeerID]*gossipMemberState),
		log:     log,
	}, nil
}

// gossipLocalState is the structured payload of one side of a
// push-pull exchange. From identifies the sender; Peers is its
// cache snapshot. The receiver merges what it doesn't know.
type gossipLocalState struct {
	From struct {
		PeerID    PeerID `json:"peer_id,omitempty"`
		AgentName string `json:"agent_name,omitempty"`
		Addr      string `json:"addr,omitempty"` // sender's advertised gossip listener
	} `json:"from"`
	At    time.Time `json:"at"`
	Peers []Peer    `json:"peers"`
}

// Run starts the gossip listener + push-pull loop. Blocks until
// ctx.Done().
func (g *Gossip) Run(ctx context.Context) error {
	bindHost, bindPort, err := splitBindAddr(g.cfg.BindAddr)
	if err != nil {
		return fmt.Errorf("gossip: parse bind addr: %w", err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(bindHost, strconv.Itoa(bindPort)))
	if err != nil {
		return fmt.Errorf("gossip: listen: %w", err)
	}
	g.mu.Lock()
	g.ln = ln
	g.mu.Unlock()
	g.log.Info("gossip: listening", "bind", ln.Addr().String(), "name", g.cfg.SelfPeerID)
	defer ln.Close()

	// Serve inbound exchanges until ctx cancels.
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed (shutdown)
			}
			go g.serveConn(conn)
		}
	}()

	// Initial push-pull; then every PushPullInterval to absorb
	// peers that came into view since the last attempt (mDNS
	// late-join, NAT hint poll) and to refresh member liveness.
	g.pushPull()
	interval := g.cfg.PushPullInterval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			ln.Close()
			<-serveDone
			return nil
		case <-t.C:
			g.pushPull()
		}
	}
}

// Addr returns the bound listener address (host:port). Empty
// before Run has bound the listener.
func (g *Gossip) Addr() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.ln == nil {
		return ""
	}
	return g.ln.Addr().String()
}

// advertiseAddr is the address we stamp into outbound envelopes.
func (g *Gossip) advertiseAddr() string {
	if g.cfg.AdvertiseAddr != "" {
		return g.cfg.AdvertiseAddr
	}
	if a := g.Addr(); a != "" {
		return a
	}
	return g.cfg.BindAddr
}

// serveConn handles one inbound exchange: read the peer's state,
// merge it, reply with ours.
func (g *Gossip) serveConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(gossipExchangeTimeout))
	theirs, ok := g.readState(conn)
	if !ok {
		return
	}
	g.absorb(theirs, conn.RemoteAddr())
	if err := g.writeState(conn); err != nil {
		g.log.Debug("gossip: reply write failed", "peer", conn.RemoteAddr().String(), "err", err)
	}
}

// pushPull dials every bootstrap address and runs one exchange.
// Failures are logged but not fatal — gossip works as a
// single-node sink even before bootstrap succeeds.
func (g *Gossip) pushPull() {
	if g.cfg.Bootstrap == nil {
		return
	}
	addrs := g.cfg.Bootstrap()
	seen := make(map[string]bool, len(addrs))
	for _, addr := range addrs {
		addr = strings.TrimSpace(addr)
		if addr == "" || seen[addr] || addr == g.Addr() {
			continue
		}
		seen[addr] = true
		if err := g.exchange(addr); err != nil {
			g.log.Debug("gossip: exchange failed", "addr", addr, "err", err)
		}
	}
}

// exchange dials addr and runs one push-pull: send our state, read
// theirs, merge.
func (g *Gossip) exchange(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, gossipExchangeTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(gossipExchangeTimeout))
	if err := g.writeState(conn); err != nil {
		return err
	}
	theirs, ok := g.readState(conn)
	if !ok {
		return fmt.Errorf("gossip: no state from %s", addr)
	}
	g.mu.Lock()
	g.last = time.Now()
	g.mu.Unlock()
	g.absorb(theirs, conn.RemoteAddr())
	return nil
}

// writeState sends one envelope: identity + cache snapshot.
func (g *Gossip) writeState(w io.Writer) error {
	body := gossipLocalState{At: time.Now()}
	body.From.PeerID = g.cfg.SelfPeerID
	body.From.AgentName = g.cfg.SelfAgentName
	body.From.Addr = g.advertiseAddr()
	if g.cfg.Cache != nil {
		body.Peers = g.cfg.Cache.Snapshot()
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// readState reads one newline-terminated JSON envelope, bounded by
// maxGossipStateBytes.
func (g *Gossip) readState(r io.Reader) (gossipLocalState, bool) {
	var body gossipLocalState
	line, err := bufio.NewReaderSize(io.LimitReader(r, maxGossipStateBytes), 64<<10).ReadString('\n')
	if err != nil && line == "" {
		return body, false
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &body); err != nil {
		g.log.Debug("gossip: bad remote state", "err", err)
		return body, false
	}
	return body, true
}

// absorb merges a remote envelope: records the sender as a member
// and upserts its fresh peers with gossip semantics.
func (g *Gossip) absorb(body gossipLocalState, remote net.Addr) {
	now := time.Now()
	from := body.From.PeerID
	if from != "" && from != g.cfg.SelfPeerID {
		addr := body.From.Addr
		if addr == "" {
			addr = remote.String()
		}
		host, portStr, _ := net.SplitHostPort(addr)
		port, _ := strconv.Atoi(portStr)
		g.mu.Lock()
		m := g.members[from]
		if m == nil {
			m = &gossipMemberState{}
			g.members[from] = m
		}
		m.AgentName = body.From.AgentName
		m.Addr, m.Port = host, port
		m.LastOK = now
		g.mu.Unlock()
	}
	for _, p := range body.Peers {
		// Skip stale gossip — if the source's observation is
		// older than gossipBroadcastTTL, don't pollute our cache.
		if p.ID == g.cfg.SelfPeerID {
			continue
		}
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
		g.cfg.Cache.Upsert(p)
	}
}

// Members returns the current member set (excluding self) — used
// by `outpost_gossip_edges` to expose the live peer set. State is
// derived from the age of the most recent successful exchange.
func (g *Gossip) Members() []GossipMember {
	if g == nil {
		return nil
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	// Prune long-dead entries while we hold the write lock.
	for id, m := range g.members {
		if now.Sub(m.LastOK) > gossipDeadAfter {
			delete(g.members, id)
		}
	}
	out := make([]GossipMember, 0, len(g.members))
	for id, m := range g.members {
		if id == g.cfg.SelfPeerID {
			continue
		}
		out = append(out, GossipMember{
			PeerID:    id,
			Addr:      m.Addr,
			Port:      m.Port,
			State:     memberState(now.Sub(m.LastOK)),
			AgentName: m.AgentName,
		})
	}
	return out
}

// memberState maps exchange age to the member state vocabulary
// (alive / suspect / dead).
func memberState(age time.Duration) string {
	switch {
	case age <= gossipAliveAfter:
		return "alive"
	case age <= gossipSuspectAfter:
		return "suspect"
	default:
		return "dead"
	}
}

// GossipMember is the wire shape returned by Members(). One row
// per known member (alive, suspect, or dead) excluding self.
type GossipMember struct {
	PeerID    PeerID `json:"peer_id"`
	Addr      string `json:"addr"`
	Port      int    `json:"port"`
	State     string `json:"state"` // alive / suspect / dead
	AgentName string `json:"agent_name,omitempty"`
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
