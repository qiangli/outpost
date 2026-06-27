package peerplane

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
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
	if err := s.cli.Announce(ctx, s.cfg.AgentName, "", LocalCandidates(port), nil); err != nil {
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
