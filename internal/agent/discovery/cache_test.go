package discovery

import (
	"testing"
	"time"
)

// TestCacheUpsertMerge confirms the union-on-merge contract: re-
// observing a peer through a second source unions Sources rather
// than replacing them.
func TestCacheUpsertMerge(t *testing.T) {
	c := NewCache(time.Minute)
	p1 := Peer{
		ID:        "SHA256:a",
		AgentName: "first",
		Sources:   []Source{SourceMDNS},
		Trust:     TrustUnverified,
	}
	c.Upsert(p1)

	p2 := Peer{
		ID:        "SHA256:a",
		AgentName: "second", // newer name should win
		Sources:   []Source{SourceHTTPProbe},
		Trust:     TrustTOFU,
	}
	merged := c.Upsert(p2)

	if merged.AgentName != "second" {
		t.Errorf("AgentName = %q, want 'second' (newer obs wins)", merged.AgentName)
	}
	if merged.Trust != TrustTOFU {
		t.Errorf("Trust = %q, want tofu (promoted)", merged.Trust)
	}
	if !containsSource(merged.Sources, SourceMDNS) || !containsSource(merged.Sources, SourceHTTPProbe) {
		t.Errorf("Sources missing union: %v", merged.Sources)
	}
}

// TestCacheTTLEviction verifies that a Snapshot after the TTL has
// elapsed drops the stale entry.
func TestCacheTTLEviction(t *testing.T) {
	c := NewCache(10 * time.Millisecond)
	c.Upsert(Peer{ID: "SHA256:x", AgentName: "x", LastSeenAt: time.Now()})
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
	time.Sleep(15 * time.Millisecond)
	if c.Len() != 0 {
		t.Errorf("Len after TTL = %d, want 0", c.Len())
	}
}

// TestCacheSnapshotStableOrder pins the deterministic ordering used
// by `outpost peers list` so CLI output doesn't flap.
func TestCacheSnapshotStableOrder(t *testing.T) {
	c := NewCache(time.Minute)
	c.Upsert(Peer{ID: "SHA256:c", AgentName: "zeta"})
	c.Upsert(Peer{ID: "SHA256:a", AgentName: "alpha"})
	c.Upsert(Peer{ID: "SHA256:b", AgentName: "beta"})
	snap := c.Snapshot()
	if len(snap) != 3 || snap[0].AgentName != "alpha" || snap[2].AgentName != "zeta" {
		t.Errorf("snapshot order wrong: %+v", snap)
	}
}

// TestCacheConcurrent ensures the lock posture handles parallel
// upserts + reads without races. Race detector finds bugs the test
// itself can't observe directly.
func TestCacheConcurrent(t *testing.T) {
	c := NewCache(time.Minute)
	done := make(chan struct{})
	for i := range 10 {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for j := range 100 {
				c.Upsert(Peer{
					ID:         PeerID(rune('A' + i)),
					AgentName:  "p",
					LastSeenAt: time.Now(),
					Sources:    []Source{SourceMDNS},
				})
				_ = j
			}
			_ = c.Snapshot()
		}(i)
	}
	for range 10 {
		<-done
	}
}
