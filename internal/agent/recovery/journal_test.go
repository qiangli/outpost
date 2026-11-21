package recovery

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJournalBoundsAndSafeReasons(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	first := time.Now().UTC().Truncate(time.Second)
	for i := range MaxEvents + 3 {
		if err := appendEvent(Event{first.Add(time.Duration(i) * time.Second), "tunnel", "down", "transport_lost"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := appendEvent(Event{first, "ssh_client", "blocked", "response body containing credentials"}); err == nil {
		t.Fatal("free-form reason was accepted")
	}
	events, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != MaxEvents || !events[0].At.Equal(first.Add(3*time.Second)) {
		t.Fatalf("expected latest %d entries, got %d", MaxEvents, len(events))
	}
	info, err := os.Stat(filepath.Join(root, "outpost", FileName))
	if err != nil || info.Size() > MaxBytes {
		t.Fatalf("evidence file cap not met: %v, %v", info, err)
	}
}
