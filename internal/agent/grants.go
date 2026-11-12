package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// GrantRule is one host-grant rule: circles AND (group OR email). Every
// field is optional; see RuleAdmits for the exact semantics.
type GrantRule struct {
	Circles []string `json:"circles"`
	Groups  []string `json:"groups"`
	Emails  []string `json:"emails"`
}

// GrantMember is one of the host owner's active, mutual members and the
// single circle the owner placed them in.
type GrantMember struct {
	Email  string `json:"email"`
	Circle string `json:"circle"`
}

// GrantCopy is the host-grant copy a satellite serves to an attached host
// at GET /api/v1/hosts/self/grants. It is relationships only — no secret —
// and identical through every satellite the host attaches to.
type GrantCopy struct {
	Host     string                 `json:"host"`
	Owner    string                 `json:"owner"`
	Revision int64                  `json:"revision"`
	Members  []GrantMember          `json:"members"`
	Groups   map[string][]string    `json:"groups"`
	Apps     map[string][]GrantRule `json:"apps"`
}

// RuleAdmits reports whether a non-owner caller matches one rule. member
// is false when the caller is not an active mutual member of the owner;
// circle is the member's circle; groups are the owner's groups the caller
// is in. A non-member never matches.
func RuleAdmits(r GrantRule, email string, member bool, circle string, groups []string) bool {
	if !member {
		return false
	}
	if len(r.Circles) > 0 && !slices.Contains(r.Circles, circle) {
		return false
	}
	if len(r.Groups) == 0 && len(r.Emails) == 0 {
		return true
	}
	for _, g := range r.Groups {
		if slices.Contains(groups, g) {
			return true
		}
	}
	return slices.ContainsFunc(r.Emails, func(e string) bool { return strings.EqualFold(e, email) })
}

// Admits reports whether email may use app under this copy: the owner
// always passes; anyone else must be a member some rule of the app admits.
// An app with no rules is owner only.
func (g *GrantCopy) Admits(app, email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || g == nil {
		return false
	}
	if strings.EqualFold(g.Owner, email) {
		return true
	}
	var circle string
	member := false
	for _, m := range g.Members {
		if strings.EqualFold(m.Email, email) {
			member, circle = true, m.Circle
			break
		}
	}
	if !member {
		return false
	}
	var groups []string
	for name, emails := range g.Groups {
		if slices.ContainsFunc(emails, func(e string) bool { return strings.EqualFold(e, email) }) {
			groups = append(groups, name)
		}
	}
	for _, r := range g.Apps[app] {
		if RuleAdmits(r, email, true, circle, groups) {
			return true
		}
	}
	return false
}

// HostGrants holds the newest grant copy received from any source.
// Until a copy arrives the host grant admits the owner only — the paired
// account email the daemon was configured with.
type HostGrants struct {
	mu    sync.RWMutex
	copy  *GrantCopy
	owner string
}

// NewHostGrants returns an empty store; owner is the host's own paired
// account email, used only while no copy has been received.
func NewHostGrants(owner string) *HostGrants {
	return &HostGrants{owner: strings.ToLower(strings.TrimSpace(owner))}
}

// Update keeps c when it is newer (higher revision) than the held copy.
func (h *HostGrants) Update(c *GrantCopy) bool {
	if c == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.copy != nil && c.Revision <= h.copy.Revision {
		return false
	}
	h.copy = c
	return true
}

// Copy returns the held copy (nil when none has been received).
func (h *HostGrants) Copy() *GrantCopy {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.copy
}

// Allow is the cloud-side gate for require_login apps. A nil store admits
// nobody.
func (h *HostGrants) Allow(app, user string) bool {
	if h == nil {
		return false
	}
	user = strings.ToLower(strings.TrimSpace(user))
	if user == "" {
		return false
	}
	h.mu.RLock()
	c, owner := h.copy, h.owner
	h.mu.RUnlock()
	if c != nil && c.Owner != "" {
		owner = c.Owner
	}
	if owner != "" && strings.EqualFold(owner, user) {
		return true
	}
	return c.Admits(app, user)
}

// GrantSource is one server the copy is pulled from: the legacy pairing
// or a satellite attachment.
type GrantSource struct {
	Name     string // satellite id or legacy:<host>, for logs
	Endpoint string
	Token    string
}

// GrantPuller pulls the host-grant copy from every source at startup
// and every Interval. A failed pull keeps the last good copy.
type GrantPuller struct {
	Sources  []GrantSource
	Store    *HostGrants
	Client   *http.Client
	Interval time.Duration
}

// DefaultGrantPullInterval is how often the copy is refreshed.
const DefaultGrantPullInterval = 30 * time.Second

// Run pulls until ctx is cancelled. It never returns an error: the copy is
// an access cache and a dead satellite must not take the daemon down.
func (p *GrantPuller) Run(ctx context.Context) error {
	iv := p.Interval
	if iv <= 0 {
		iv = DefaultGrantPullInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		p.PullOnce(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// PullOnce fetches from every source and keeps the newest copy.
func (p *GrantPuller) PullOnce(ctx context.Context) {
	for _, s := range p.Sources {
		c, err := p.fetch(ctx, s)
		if err != nil {
			slog.Warn("host grants: pull failed; keeping last copy", "source", s.Name, "err", err)
			continue
		}
		p.Store.Update(c)
	}
}

func (p *GrantPuller) fetch(ctx context.Context, s GrantSource) (*GrantCopy, error) {
	cl := p.Client
	if cl == nil {
		cl = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.Endpoint, "/")+"/api/v1/hosts/self/grants", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var c GrantCopy
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&c); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &c, nil
}
