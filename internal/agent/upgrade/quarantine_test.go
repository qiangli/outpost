package upgrade

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQuarantineRoundTrip(t *testing.T) {
	q := NewQuarantine(filepath.Join(t.TempDir(), "q.json"))

	if q.Has("rel-1") {
		t.Fatal("empty quarantine should not have rel-1")
	}
	if err := q.Add(QuarantineEntry{ReleaseID: "rel-1", Commit: "abc1234", Reason: "boom"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !q.Has("rel-1") {
		t.Fatal("rel-1 should be quarantined after Add")
	}
	if q.Has("rel-2") {
		t.Fatal("rel-2 was never added")
	}

	list, err := q.List()
	if err != nil || len(list) != 1 || list[0].ReleaseID != "rel-1" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if list[0].RevertedAt.IsZero() {
		t.Fatal("RevertedAt should be stamped")
	}

	if err := q.Clear("rel-1"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if q.Has("rel-1") {
		t.Fatal("rel-1 should be gone after Clear")
	}

	_ = q.Add(QuarantineEntry{ReleaseID: "a"})
	_ = q.Add(QuarantineEntry{ReleaseID: "b"})
	if err := q.ClearAll(); err != nil {
		t.Fatalf("ClearAll: %v", err)
	}
	if l, _ := q.List(); len(l) != 0 {
		t.Fatalf("ClearAll left %d entries", len(l))
	}
}

func TestQuarantineFailsOpenOnCorrupt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "q.json")
	if err := os.WriteFile(p, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	q := NewQuarantine(p)
	// A corrupt quarantine file must NOT wedge upgrades — Has fails open.
	if q.Has("anything") {
		t.Fatal("corrupt quarantine should fail open (Has=false)")
	}
}

func TestQuarantineNilSafe(t *testing.T) {
	var q *Quarantine
	if q.Has("x") {
		t.Fatal("nil quarantine Has must be false")
	}
}
