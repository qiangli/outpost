// Package peerhosts caches the list of paired outpost hostnames as
// returned by cloudbox's /api/v1/ssh/hosts endpoint. It is consumed by
// the SSH server's `direct-tcpip` allowlist so `ssh -J peerA peerB`
// works between paired hosts without widening trust to arbitrary
// destinations.
//
// The registry refreshes on a TTL (default 5 min). On cloudbox-side
// failure it serves the last good snapshot rather than denying — the
// trust model still rests on (1) the inner SSH handshake's OS-password
// gate and (2) the loopback-only fallback when no snapshot is available
// at all.
package peerhosts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultTTL     = 5 * time.Minute
	defaultTimeout = 10 * time.Second
)

// Config is the dial information the registry uses to query cloudbox.
// Token is the per-user access_token cloudbox issued at register time
// (fc.AccessToken). An empty Token disables the registry — it will
// answer false to every IsPeer query, which keeps the loopback-only
// posture for unpaired outposts.
type Config struct {
	ServerAddr  string
	ServerPort  int
	Protocol    string
	Token       string
	TTL         time.Duration
	HTTPTimeout time.Duration
}

// Registry holds a cached set of peer hostnames. Safe for concurrent
// use. Zero-value Registry is a no-op (IsPeer always false) so callers
// that don't yet have an AccessToken can pass nil or the zero value
// safely.
type Registry struct {
	cfg Config

	mu     sync.Mutex
	hosts  map[string]struct{}
	loaded time.Time
}

// New returns a Registry configured for cfg. An empty cfg.Token yields
// a no-op registry (IsPeer always false) — useful for unpaired
// outposts.
func New(cfg Config) *Registry {
	if cfg.TTL == 0 {
		cfg.TTL = defaultTTL
	}
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = defaultTimeout
	}
	return &Registry{cfg: cfg}
}

// IsPeer reports whether host is a paired outpost in this account.
// Refreshes the cached list when older than TTL; on refresh failure
// keeps serving the last good snapshot. Returns false when no snapshot
// has ever loaded successfully (initial cloudbox outage) — the
// caller's loopback-only fallback covers that case.
func (r *Registry) IsPeer(ctx context.Context, host string) bool {
	if r == nil || strings.TrimSpace(r.cfg.Token) == "" {
		return false
	}
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}

	r.mu.Lock()
	stale := r.hosts == nil || time.Since(r.loaded) > r.cfg.TTL
	r.mu.Unlock()

	if stale {
		_ = r.refresh(ctx) // ignore err: we'll serve stale if we have it
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hosts == nil {
		return false
	}
	_, ok := r.hosts[h]
	return ok
}

// Refresh forces a cache refresh. Returned error is for callers that
// want to surface cloudbox-side trouble (e.g. an admin probe); the
// normal IsPeer caller doesn't care.
func (r *Registry) Refresh(ctx context.Context) error { return r.refresh(ctx) }

func (r *Registry) refresh(ctx context.Context) error {
	u, err := r.endpoint()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, r.cfg.HTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.cfg.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Debug("peerhosts: refresh failed", "err", err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		slog.Debug("peerhosts: refresh non-200",
			"status", resp.StatusCode, "body", strings.TrimSpace(string(body)))
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Hosts []struct {
			Host string `json:"host"`
		} `json:"hosts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	set := make(map[string]struct{}, len(payload.Hosts))
	for _, h := range payload.Hosts {
		name := strings.ToLower(strings.TrimSpace(h.Host))
		if name == "" {
			continue
		}
		set[name] = struct{}{}
	}
	r.mu.Lock()
	r.hosts = set
	r.loaded = time.Now()
	r.mu.Unlock()
	return nil
}

func (r *Registry) endpoint() (string, error) {
	s := strings.TrimSpace(r.cfg.ServerAddr)
	if s == "" {
		return "", fmt.Errorf("peerhosts: empty ServerAddr")
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(strings.TrimSpace(r.cfg.Protocol), "wss") ||
		strings.EqualFold(u.Scheme, "https") {
		u.Scheme = "https"
	} else {
		u.Scheme = "http"
	}
	if u.Port() == "" && r.cfg.ServerPort > 0 {
		u.Host = u.Hostname() + ":" + strconv.Itoa(r.cfg.ServerPort)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/ssh/hosts"
	return u.String(), nil
}
