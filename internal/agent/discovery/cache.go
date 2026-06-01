// Discovery cache: in-memory snapshot of currently-known peers,
// merged from every discovery source (mDNS browse, HTTP probes,
// cloudbox NAT hints, gossip). The daemon owns one instance;
// outpost://peers + outpost peers list read from it; the
// observation ticker snapshots it every 5 min.
//
// Eviction policy: any peer not refreshed within `TTL` is dropped.
// Defaults are conservative — 15 minutes covers a browse miss or
// two without flapping. Tests can shrink it.
//
// Concurrency: safe for concurrent reads + writes. Single mutex; the
// cache is small (<256 entries in normal operation) so finer-grained
// locking would be overkill.
package discovery

import (
	"slices"
	"sort"
	"sync"
	"time"
)

// DefaultCacheTTL is how long a peer stays in the cache after its
// most-recent observation. 15 min absorbs a typical browse miss
// (browse cadence ~30s) without holding ghost entries forever.
const DefaultCacheTTL = 15 * time.Minute

// Cache holds the live set of known peers, keyed by PeerID.
//
// HyParView bounds (roadmap item #16): active / passive view caps
// set via SetBounds. Enforced by the periodic Compact goroutine —
// Upsert itself stays O(1) and unaware of partitions, so the hot
// path doesn't pay any extra cost.
type Cache struct {
	ttl time.Duration

	mu   sync.RWMutex
	byID map[PeerID]*Peer

	// HyParView active/passive view bounds. 0 = use defaults
	// (DefaultActiveMax / DefaultPassiveMax). Operator override
	// via Cache.SetBounds.
	activeMax  int
	passiveMax int
}

// NewCache returns an empty cache. TTL == 0 means use the default.
func NewCache(ttl time.Duration) *Cache {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &Cache{
		ttl:  ttl,
		byID: make(map[PeerID]*Peer),
	}
}

// Upsert merges `p` into the cache. When a peer with the same ID
// already exists, its Sources union with p.Sources, its endpoint
// list is replaced with p.Endpoints (the most recent observation
// wins — we don't keep historical endpoints), and LastSeenAt
// becomes the newer of the two.
//
// Returns the merged Peer (a fresh value, not aliased to caller's
// argument) for callers that want to use the post-merge state.
func (c *Cache) Upsert(p Peer) Peer {
	if p.ID == "" {
		return p
	}
	if p.LastSeenAt.IsZero() {
		p.LastSeenAt = time.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	existing, ok := c.byID[p.ID]
	if !ok {
		// Defensive copy: caller may mutate `p` after the call.
		cp := p
		c.byID[p.ID] = &cp
		return cp
	}
	// Merge sources.
	for _, s := range p.Sources {
		if !containsSource(existing.Sources, s) {
			existing.Sources = append(existing.Sources, s)
		}
	}
	if len(p.Endpoints) > 0 {
		existing.Endpoints = p.Endpoints
	}
	if p.AgentName != "" {
		existing.AgentName = p.AgentName
	}
	if p.AssignedHostname != "" {
		existing.AssignedHostname = p.AssignedHostname
	}
	if p.OAuth2Email != "" {
		existing.OAuth2Email = p.OAuth2Email
	}
	if p.OSUsername != "" {
		existing.OSUsername = p.OSUsername
	}
	if p.Version != "" {
		existing.Version = p.Version
	}
	if p.CloudboxBase != "" {
		existing.CloudboxBase = p.CloudboxBase
	}
	if p.Paired {
		existing.Paired = true
	}
	if trustRank(p.Trust) > trustRank(existing.Trust) {
		existing.Trust = p.Trust
	}
	if p.LastSeenAt.After(existing.LastSeenAt) {
		existing.LastSeenAt = p.LastSeenAt
	}
	return *existing
}

// Snapshot returns a copy of all currently-live cache entries.
// Stale entries (older than TTL) are evicted as a side effect, so
// the returned slice reflects only fresh observations.
func (c *Cache) Snapshot() []Peer {
	now := time.Now()
	c.mu.Lock()
	c.evictStaleLocked(now)
	out := make([]Peer, 0, len(c.byID))
	for _, p := range c.byID {
		out = append(out, *p)
	}
	c.mu.Unlock()
	// Stable order so CLI output is deterministic.
	sort.Slice(out, func(i, j int) bool {
		if out[i].AgentName == out[j].AgentName {
			return out[i].ID < out[j].ID
		}
		return out[i].AgentName < out[j].AgentName
	})
	return out
}

// SnapshotIDs is the cheap "just the keys" path the observation
// ticker uses every 5 min. Doesn't allocate the full Peer values.
func (c *Cache) SnapshotIDs() []PeerID {
	now := time.Now()
	c.mu.Lock()
	c.evictStaleLocked(now)
	out := make([]PeerID, 0, len(c.byID))
	for id := range c.byID {
		out = append(out, id)
	}
	c.mu.Unlock()
	return out
}

// Len reports the current live entry count (after evicting stale).
// Cheap; for tests.
func (c *Cache) Len() int {
	now := time.Now()
	c.mu.Lock()
	c.evictStaleLocked(now)
	n := len(c.byID)
	c.mu.Unlock()
	return n
}

// evictStaleLocked drops every entry whose LastSeenAt is older than
// now-TTL. Caller must hold c.mu.
func (c *Cache) evictStaleLocked(now time.Time) {
	cutoff := now.Add(-c.ttl)
	for id, p := range c.byID {
		if p.LastSeenAt.Before(cutoff) {
			delete(c.byID, id)
		}
	}
}

// containsSource is a tiny helper for the Upsert merge — slices are
// always small (≤4 typical) so linear scan is fine.
func containsSource(haystack []Source, needle Source) bool {
	return slices.Contains(haystack, needle)
}

// trustRank gives the trust levels a total order so the cache merge
// can promote (never demote) on re-observation. Unknown values rank
// at -1 so they never beat a recognized level.
func trustRank(t TrustLevel) int {
	switch t {
	case TrustUnverified:
		return 0
	case TrustTOFU:
		return 1
	case TrustCloudboxCert:
		return 2
	}
	return -1
}
