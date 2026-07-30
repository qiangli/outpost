package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuarantineStaleIPAMMovesEvidenceAndResetsAllocation(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ipam")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "erased-sandbox.ip"), []byte("10.42.1.2"), 0o600); err != nil {
		t.Fatal(err)
	}

	moved, err := quarantineStaleIPAM(dir, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC), 3)
	if err != nil {
		t.Fatalf("quarantineStaleIPAM() error = %v", err)
	}
	if !strings.HasPrefix(moved, filepath.Join(root, "ipam-quarantine", "ipam-")) {
		t.Fatalf("moved path = %q", moved)
	}
	if got, err := os.ReadFile(filepath.Join(moved, "erased-sandbox.ip")); err != nil || string(got) != "10.42.1.2" {
		t.Fatalf("quarantined evidence = %q, err=%v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("fresh IPAM directory is not empty: %v", entries)
	}
	ip, err := allocateIP(dir, "10.42.1.0/24", "new-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if got := ip.String(); got != "10.42.1.2" {
		t.Fatalf("first post-recreation allocation = %s, want 10.42.1.2", got)
	}
}

func TestQuarantineStaleIPAMRetentionIsBounded(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ipam")
	base := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "lease.ip"), []byte("10.42.1.2"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := quarantineStaleIPAM(dir, base.Add(time.Duration(i)*time.Second), 3); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "ipam-quarantine"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("retained quarantines = %d, want 3", len(entries))
	}
	for _, entry := range entries {
		if entry.Name() == "ipam-20260729T120000.000000000Z" ||
			entry.Name() == "ipam-20260729T120001.000000000Z" {
			t.Fatalf("old quarantine was retained: %s", entry.Name())
		}
	}
}

func TestQuarantineStaleIPAMRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "ipam")); err != nil {
		t.Fatal(err)
	}
	if _, err := quarantineStaleIPAM(filepath.Join(root, "ipam"), time.Now(), 3); err == nil {
		t.Fatal("symlink IPAM directory was accepted")
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "keep" {
		t.Fatalf("outside sentinel changed: %q err=%v", got, err)
	}
}
