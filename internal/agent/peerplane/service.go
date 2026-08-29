package peerplane

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/outpost/internal/agent/peerstatus"
)

// Config wires the peer-plane service to cloudbox.
type Config struct {
	AgentName   string // this host's name (self)
	CloudboxURL string
	AccessToken string
	HTTPClient  *http.Client
	Interval    time.Duration // probe cadence; 0 → default 60s
	Logger      *slog.Logger
}

// PeerTier is the latest measured locality of a peer link. Tier is GROUND
// TRUTH (measured RTT); SameLANHint is cloudbox's egress-IP guess, kept only so
// operators can see where the heuristic disagrees with the measurement.
type PeerTier struct {
	Host        string    `json:"host"`
	Tier        Tier      `json:"tier"`
	RTT         float64   `json:"rtt_ms"`
	Addr        string    `json:"addr"`
	SameLANHint bool      `json:"egress_same_lan_hint"`
	At          time.Time `json:"at"`
}

// Service announces this host's candidates to cloudbox, runs a probe responder,
// and periodically measures + tiers every reachable peer.
type Service struct {
	cfg Config
	cli *Client
	log *slog.Logger

	mu    sync.Mutex
	tiers map[string]PeerTier

	// port is the echo responder's bound UDP port once Run has started it
	// (0 before). Guarded by mu; read by Candidates().
	port int

	// identity, when set, supplies the mesh data plane's peer id + dialable
	// multiaddrs so the RTT prober CO-ANNOUNCES them instead of publishing a
	// blank peer id. cloudbox keeps ONE PeerNode row per host and its Announce
	// overwrites peer_id + candidates wholesale on every call, so an announce
	// that omits the mesh identity erases what the mesh rendezvous published a
	// moment earlier — and /api/v1/peer/resolve drops any row whose peer_id is
	// empty. With two independent 60s announcers racing on the same row, a
	// host's mesh services (`ssh`, `git`, …) were resolvable only in the window
	// between the mesh tick and the next prober tick, which is exactly the
	// intermittent `mesh dial ssh: no reachable peer exposes service` seen on
	// hosts running both. Guarded by mu.
	identity func() (peerID string, candidates []string)
}

// New builds the service.
func New(cfg Config) *Service {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		cfg:   cfg,
		cli:   &Client{BaseURL: cfg.CloudboxURL, Token: cfg.AccessToken, HC: cfg.HTTPClient},
		log:   log,
		tiers: map[string]PeerTier{},
	}
}

// SetIdentity installs the mesh identity provider the prober co-announces
// (see Service.identity). nil clears it. Safe to call before or after Run.
func (s *Service) SetIdentity(fn func() (peerID string, candidates []string)) {
	s.mu.Lock()
	s.identity = fn
	s.mu.Unlock()
}

// Candidates returns this host's RTT-probe candidates ("ip:port" of the echo
// responder) — empty until Run has bound the responder. The mesh rendezvous
// folds these into ITS announce so the two announcers publish the same
// candidate set instead of overwriting each other's.
func (s *Service) Candidates() []string {
	s.mu.Lock()
	port := s.port
	s.mu.Unlock()
	if port <= 0 {
		return nil
	}
	return LocalCandidates(port)
}

// announcement composes what this cycle publishes to cloudbox: the mesh peer
// id (when a provider is installed) and the union of the mesh's dialable
// multiaddrs + our own probe candidates, de-duplicated and sorted so a
// steady-state announce is byte-stable.
func (s *Service) announcement(port int) (peerID string, candidates []string) {
	s.mu.Lock()
	fn := s.identity
	s.mu.Unlock()
	var meshCands []string
	if fn != nil {
		peerID, meshCands = fn()
	}
	return peerID, MergeCandidates(meshCands, LocalCandidates(port))
}

// MergeCandidates unions candidate lists (mesh multiaddrs, probe "ip:port"s),
// dropping blanks and duplicates, sorted for a stable wire form.
func MergeCandidates(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, c := range l {
			c = strings.TrimSpace(c)
			if c == "" || seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// probeCandidates keeps only the "host:port" entries a UDP echo probe can
// address. The shared PeerNode row now also carries the mesh's multiaddrs
// ("/ip4/…/udp/…/quic-v1"); those are for libp2p, not for the prober, and
// each would otherwise burn a full probe timeout before reporting unreached.
func probeCandidates(cands []string) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		c = strings.TrimSpace(c)
		if c == "" || strings.HasPrefix(c, "/") {
			continue
		}
		out = append(out, c)
	}
	return out
}

// Run starts the responder + the announce/probe loop until ctx is done. No-op
// (returns nil) when unpaired.
func (s *Service) Run(ctx context.Context) error {
	if s.cfg.AccessToken == "" || s.cfg.CloudboxURL == "" {
		s.log.Warn("peerplane: disabled (no access token / cloudbox URL)")
		return nil
	}
	resp, err := NewEchoResponder(0)
	if err != nil {
		return err
	}
	go resp.Run(ctx)
	port := resp.Port()
	s.mu.Lock()
	s.port = port
	s.mu.Unlock()
	s.log.Info("peerplane: probe responder up", "port", port, "candidates", LocalCandidates(port))

	interval := s.cfg.Interval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	s.cycle(ctx, port)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			s.cycle(ctx, port)
		}
	}
}

// cycle announces our candidates, reciprocates inbound rendezvous, and actively
// probes every online peer.
func (s *Service) cycle(ctx context.Context, port int) {
	// nil services: the RTT prober preserves whatever the mesh announcer set.
	// peer id + candidates are the COMPOSITE (mesh identity + probe addrs) for
	// the same reason — see Service.identity.
	peerID, cands := s.announcement(port)
	if err := s.cli.Announce(ctx, s.cfg.AgentName, peerID, cands, nil); err != nil {
		s.log.Debug("peerplane: announce failed", "err", err)
	}

	// Reciprocate inbound rendezvous: probe whoever asked to connect to us.
	if box, err := s.cli.Inbox(ctx, s.cfg.AgentName); err == nil {
		for _, rz := range box {
			s.measure(rz.FromHost, rz.FromCandidates, false)
		}
	}

	// Active discovery: enumerate peers (cloudbox status board), request a
	// rendezvous with each, and probe its candidates.
	peers, err := peerstatus.Fetch(ctx, s.cfg.CloudboxURL, s.cfg.AccessToken, s.cfg.HTTPClient)
	if err != nil {
		s.log.Debug("peerplane: peer list failed", "err", err)
		return
	}
	for _, p := range peers {
		if !p.Online || p.Host == s.cfg.AgentName {
			continue
		}
		tgt, err := s.cli.Connect(ctx, s.cfg.AgentName, p.Host)
		if err != nil {
			continue // peer hasn't announced / not reachable for rendezvous yet
		}
		s.measure(p.Host, tgt.Peer.Candidates, tgt.SameLAN)
	}
}

// measure probes a peer's candidates and records the best (lowest-RTT) tier.
func (s *Service) measure(host string, cands []string, sameLANHint bool) {
	cands = probeCandidates(cands)
	if host == "" || len(cands) == 0 {
		return
	}
	_, best := ProbeAll(cands, 4, ProbeCandidate)
	pt := PeerTier{Host: host, Tier: TierUnreached, SameLANHint: sameLANHint, At: time.Now().UTC()}
	if best != nil {
		pt.Tier, pt.RTT, pt.Addr = best.Tier, best.RTT, best.Addr
	}
	s.mu.Lock()
	s.tiers[host] = pt
	s.mu.Unlock()
	s.log.Info("peerplane: measured peer",
		"host", host, "tier", pt.Tier, "rtt_ms", pt.RTT, "addr", pt.Addr, "egress_same_lan", sameLANHint)
}

// Snapshot returns the latest measured tiers, host-sorted. Surfaced via status
// / MCP so operators see ground-truth locality vs. the egress hint.
func (s *Service) Snapshot() []PeerTier {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PeerTier, 0, len(s.tiers))
	for _, v := range s.tiers {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}
