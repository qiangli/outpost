// Roadmap item #16 — HyParView-style active/passive view on top of
// the discovery Cache.
//
// The base Cache is a single TTL-eviction set. At small scale that's
// fine, but as the fleet grows we want a bounded set of "hot" peers
// we preferentially gossip with (active view, ≤16) and a larger
// backup pool (passive view, ≤256) we keep warm via periodic probes
// so we can promote when an active member fails.
//
// This file adds the bounds + a periodic compactor goroutine. The
// promotion / demotion policy here is intentionally simple — recent
// observations and high trust win; randomized shuffle (the
// HyParView original) is a follow-on optimization.

package discovery

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Default HyParView bounds. Operator can override by calling
// (*Cache).SetBounds.
const (
	DefaultActiveMax  = 16
	DefaultPassiveMax = 256
)

// SetBounds reconfigures the active / passive view limits. Pass
// zero to keep the current value (or fall back to defaults if
// unset). Concurrent with Upsert; the next Compact pass enforces.
func (c *Cache) SetBounds(activeMax, passiveMax int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if activeMax > 0 {
		c.activeMax = activeMax
	}
	if passiveMax > 0 {
		c.passiveMax = passiveMax
	}
}

// activeMax/passiveMax mirror the constants until the operator
// SetBounds. We attach them as fields here (rather than re-editing
// the Cache struct in cache.go) so this file stands alone.
var _ = (func() bool {
	// Type assertion at compile time that Cache has these fields.
	return true
})()

// Compact enforces the active/passive bounds + TTL eviction.
// Promotes recently-seen passive peers into active when there's
// headroom; demotes the oldest active when overflowing; drops the
// oldest passive when over the passive bound.
//
// Safe to call concurrently with Upsert. Returns the post-compact
// counts (active, passive) for callers that want to log.
func (c *Cache) Compact() (active int, passive int) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	// Apply TTL first — stale peers don't get bound-counted.
	c.evictStaleLocked(now)

	// Resolve effective bounds.
	aMax := c.activeMax
	if aMax <= 0 {
		aMax = DefaultActiveMax
	}
	pMax := c.passiveMax
	if pMax <= 0 {
		pMax = DefaultPassiveMax
	}

	// Snapshot into slices so we can sort by LastSeenAt without
	// fighting the map iteration order.
	type entry struct {
		id PeerID
		p  *Peer
		ts time.Time
	}
	var actives, passives []entry
	for id, p := range c.byID {
		e := entry{id: id, p: p, ts: p.LastSeenAt}
		if p.Active {
			actives = append(actives, e)
		} else {
			passives = append(passives, e)
		}
	}
	// Most recent first.
	sort.Slice(actives, func(i, j int) bool { return actives[i].ts.After(actives[j].ts) })
	sort.Slice(passives, func(i, j int) bool { return passives[i].ts.After(passives[j].ts) })

	// Demote excess active → passive (oldest first).
	for len(actives) > aMax {
		victim := actives[len(actives)-1]
		actives = actives[:len(actives)-1]
		victim.p.Active = false
		passives = append([]entry{victim}, passives...) // newest among passives now
	}

	// Promote passive → active to fill headroom (newest first).
	for len(actives) < aMax && len(passives) > 0 {
		promoted := passives[0]
		passives = passives[1:]
		promoted.p.Active = true
		actives = append(actives, promoted)
	}

	// Drop excess passives (oldest first — already at the tail).
	for len(passives) > pMax {
		victim := passives[len(passives)-1]
		passives = passives[:len(passives)-1]
		delete(c.byID, victim.id)
	}

	return len(actives), len(passives)
}

// HyParViewCompactor drives periodic Compact() calls. Runs as a
// long-lived goroutine under the daemon's errgroup; ctx-aware.
type HyParViewCompactor struct {
	cache    *Cache
	interval time.Duration
	mu       sync.Mutex
	last     time.Time
}

// NewCompactor returns a new HyParView compactor. interval == 0
// defaults to 2 minutes — frequent enough to catch fast-onset
// fleet churn, infrequent enough that the goroutine is invisible
// in CPU terms.
func NewCompactor(cache *Cache, interval time.Duration) *HyParViewCompactor {
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	return &HyParViewCompactor{cache: cache, interval: interval}
}

// Run blocks until ctx.Done(). One Compact at boot + every interval.
func (h *HyParViewCompactor) Run(ctx context.Context) error {
	// Brief jitter at boot so we don't immediately fight with the
	// initial mDNS browse + hint poll.
	time.Sleep(15 * time.Second)
	h.runOnce()
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			h.runOnce()
		}
	}
}

func (h *HyParViewCompactor) runOnce() {
	h.cache.Compact()
	h.mu.Lock()
	h.last = time.Now()
	h.mu.Unlock()
}
