// Package apphealth provides per-app reachability measurement over
// outpost-owned TCP/HTTP paths (no ICMP/raw sockets). It probes each
// registered app and records reachable + RTT, classified into the same
// tier model (tp/lan/wan/unreached) the peer-plane uses.
package apphealth

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/qiangli/outpost/internal/agent"
)

// Tier thresholds (round-trip ms). Same values as peerplane so the
// measured tiers are directly comparable across app health and peer
// locality.
const (
	tpMaxRTT  = 2.0
	lanMaxRTT = 20.0
)

// Tier strings match peerplane.Tier so both views share a single
// vocabulary in the admin UI and MCP tools.
const (
	TierTP        = "tp"
	TierLAN       = "lan"
	TierWAN       = "wan"
	TierUnreached = "unreached"
)

// Classify maps a measured RTT (ms) to a tier.
func Classify(rttMS float64) string {
	switch {
	case rttMS <= tpMaxRTT:
		return TierTP
	case rttMS <= lanMaxRTT:
		return TierLAN
	default:
		return TierWAN
	}
}

// ProbeResult is one app's reachability measurement.
type ProbeResult struct {
	Name       string    `json:"name"`
	Scheme     string    `json:"scheme"`
	Target     string    `json:"target"`
	Reachable  bool      `json:"reachable"`
	RTTms      float64   `json:"rtt_ms"`
	Tier       string    `json:"tier"`
	StatusCode int       `json:"status_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	At         time.Time `json:"at"`
}

// Config wires the app-health service.
type Config struct {
	// Apps is the live registry — the service reads Entries() each cycle.
	Apps *agent.AppRegistry

	// HTTPClient, when set, is used for HTTP probes. When nil, a
	// default client with a 5s timeout is used.
	HTTPClient *http.Client

	// Logger, when set, receives probe result logs. Defaults to the
	// global default logger.
	Logger *slog.Logger

	// Interval between probe cycles. Zero means default (60s).
	Interval time.Duration
}

// Service periodically probes every registered app and stores the
// latest reachability + RTT per app.
type Service struct {
	cfg Config
	log *slog.Logger

	mu    sync.Mutex
	state map[string]ProbeResult
}

// New builds the service.
func New(cfg Config) *Service {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Second
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	cfg.HTTPClient = hc
	return &Service{
		cfg:   cfg,
		log:   log,
		state: map[string]ProbeResult{},
	}
}

// Run starts the periodic probe loop until ctx is done. Returns nil
// when apps is nil (no-op).
func (s *Service) Run(ctx context.Context) error {
	if s.cfg.Apps == nil {
		s.log.Warn("apphealth: disabled (no app registry)")
		return nil
	}
	s.log.Info("apphealth: probe loop started", "interval", s.cfg.Interval)
	s.cycle()
	tick := time.NewTicker(s.cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			s.cycle()
		}
	}
}

func (s *Service) cycle() {
	entries := s.cfg.Apps.Entries()
	for _, e := range entries {
		r := s.probeOne(e)
		s.mu.Lock()
		s.state[e.Name] = r
		s.mu.Unlock()
		s.log.Info("apphealth: probed app",
			"name", r.Name, "scheme", r.Scheme, "target", r.Target,
			"reachable", r.Reachable, "rtt_ms", r.RTTms, "tier", r.Tier)
	}
}

// probeOne measures reachability of one app entry. Uses HTTP GET for
// http/https/unix schemes, TCP connect for tcp scheme.
func (s *Service) probeOne(e agent.AppEntry) ProbeResult {
	at := time.Now().UTC()
	u := s.cfg.Apps.LookupTarget(e.Name)
	var target string
	if u != nil {
		target = u.String()
	} else if tcp := s.cfg.Apps.LookupTCP(e.Name); tcp != "" {
		target = tcp
	} else {
		return ProbeResult{
			Name: e.Name, Scheme: e.Scheme, Target: "",
			Tier: TierUnreached, Error: "app not registered", At: at,
		}
	}

	scheme := e.Scheme
	if scheme == "" {
		scheme = "http"
	}

	var r ProbeResult
	switch scheme {
	case "http", "https", "unix", "npipe":
		r = probeHTTP(s.cfg.HTTPClient, target)
	case "tcp":
		r = probeTCP(target)
	default:
		r = ProbeResult{Name: e.Name, Scheme: scheme, Target: target,
			Tier: TierUnreached, Error: "unsupported scheme: " + scheme, At: at}
	}
	r.Name = e.Name
	r.Scheme = scheme
	r.Target = target
	r.At = at
	return r
}

// probeHTTP measures RTT by sending a GET request and timing the
// round-trip. Reachable means a non-5xx response.
func probeHTTP(client *http.Client, target string) ProbeResult {
	t0 := time.Now()
	resp, err := client.Get(target)
	rtt := float64(time.Since(t0).Microseconds()) / 1000.0
	if err != nil {
		return ProbeResult{RTTms: rtt, Tier: TierUnreached, Error: err.Error()}
	}
	defer resp.Body.Close()
	r := ProbeResult{RTTms: rtt, StatusCode: resp.StatusCode}
	if resp.StatusCode >= 500 {
		r.Tier = TierUnreached
		r.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	} else {
		r.Reachable = true
		r.Tier = Classify(rtt)
	}
	return r
}

// probeTCP measures RTT by timing a TCP handshake to the target.
func probeTCP(addr string) ProbeResult {
	t0 := time.Now()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	rtt := float64(time.Since(t0).Microseconds()) / 1000.0
	if err != nil {
		return ProbeResult{RTTms: rtt, Tier: TierUnreached, Error: err.Error()}
	}
	c.Close()
	return ProbeResult{Reachable: true, RTTms: rtt, Tier: Classify(rtt)}
}

// Snapshot returns the latest result for every app, sorted by name.
func (s *Service) Snapshot() []ProbeResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ProbeResult, 0, len(s.state))
	for _, v := range s.state {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
