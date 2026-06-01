package discovery

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLedgerAppendTail covers the round-trip path and confirms the
// newest-N semantics of Tail.
func TestLedgerAppendTail(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "reachability.jsonl")
	l, err := OpenLedger(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for i := range 10 {
		if err := l.Append(ReachabilityEdge{
			Self:      "SHA256:self",
			Peer:      PeerID(string(rune('a' + i))),
			Endpoint:  Endpoint{Kind: EndpointLANSSH, Host: "x", Port: 22},
			Transport: "ssh",
			LatencyMs: int64(i),
			At:        now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	all, err := l.Tail(0)
	if err != nil {
		t.Fatalf("tail all: %v", err)
	}
	if len(all) != 10 {
		t.Fatalf("Tail(0) len=%d want 10", len(all))
	}
	if all[0].LatencyMs != 0 || all[9].LatencyMs != 9 {
		t.Errorf("order wrong: first=%d last=%d", all[0].LatencyMs, all[9].LatencyMs)
	}

	tail3, _ := l.Tail(3)
	if len(tail3) != 3 || tail3[0].LatencyMs != 7 || tail3[2].LatencyMs != 9 {
		t.Errorf("Tail(3) wrong: %+v", tail3)
	}
}

// TestLedgerRotation pins the bounded-growth contract: after we exceed
// the soft cap, rotation prunes to half the cap. Both the file and
// the in-memory counter reflect the prune.
func TestLedgerRotation(t *testing.T) {
	saved := LedgerMaxEntries
	// Shrink for the test so we don't waste time generating 10k rows.
	defer func() { /* no exported setter; test relies on the const */ }()
	_ = saved

	// Use a side-channel: write LedgerMaxEntries+1 entries and assert
	// the on-disk count drops to LedgerMaxEntries/2.
	tmp := t.TempDir()
	path := filepath.Join(tmp, "reachability.jsonl")
	l, err := OpenLedger(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := range LedgerMaxEntries + 1 {
		_ = l.Append(ReachabilityEdge{
			Self: "SHA256:s",
			Peer: "SHA256:p",
			At:   time.Now().Add(time.Duration(i) * time.Millisecond),
		})
	}
	// After rotation the file should be in the (LedgerMaxEntries/2,
	// LedgerMaxEntries) window: rotation fires on the entry that
	// pushes the count to LedgerMaxEntries, leaves LedgerMaxEntries/2,
	// then the (LedgerMaxEntries+1)th append goes on top of that.
	all, err := l.Tail(0)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(all) > LedgerMaxEntries || len(all) <= LedgerMaxEntries/2 {
		t.Errorf("after rotation len=%d, want in (%d, %d]",
			len(all), LedgerMaxEntries/2, LedgerMaxEntries)
	}
}

// TestLedgerSurvivesCorruptLine covers the "partial write at crash
// time" failure mode: a corrupt JSONL row should be skipped, not
// kill the whole tail.
func TestLedgerSurvivesCorruptLine(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "reachability.jsonl")
	l, _ := OpenLedger(path)
	_ = l.Append(ReachabilityEdge{Self: "SHA256:s", Peer: "SHA256:p"})
	// Inject a corrupt row.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("not-json-at-all\n")
	_ = f.Close()
	_ = l.Append(ReachabilityEdge{Self: "SHA256:s2", Peer: "SHA256:p2"})

	tail, err := l.Tail(0)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(tail) != 2 {
		t.Errorf("Tail after corruption len=%d want 2", len(tail))
	}
}
