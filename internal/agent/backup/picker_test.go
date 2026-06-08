package backup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, contents string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}
	return path
}

func TestPickLatest_NewestByMtime(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeFile(t, dir, "backup-old.zip", "old", now.Add(-2*time.Hour))
	writeFile(t, dir, "backup-mid.zip", "mid", now.Add(-1*time.Hour))
	want := writeFile(t, dir, "backup-new.zip", "new", now)

	got, info, err := PickLatest(dir)
	if err != nil {
		t.Fatalf("PickLatest: %v", err)
	}
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if info.Size() != 3 {
		t.Errorf("size %d, want 3", info.Size())
	}
}

func TestPickLatest_IgnoresHidden(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// Hidden file is newest — should be ignored.
	writeFile(t, dir, ".lock", "hidden", now)
	want := writeFile(t, dir, "backup.zip", "real", now.Add(-1*time.Minute))

	got, _, err := PickLatest(dir)
	if err != nil {
		t.Fatalf("PickLatest: %v", err)
	}
	if got != want {
		t.Errorf("got %s, expected hidden file to be ignored", got)
	}
}

func TestPickLatest_IgnoresSubdirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "stale"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	now := time.Now()
	want := writeFile(t, dir, "backup.zip", "real", now)
	// File inside subdir is newer — should be ignored (no recursion).
	writeFile(t, filepath.Join(dir, "stale"), "newer.zip", "x", now.Add(1*time.Hour))

	got, _, err := PickLatest(dir)
	if err != nil {
		t.Fatalf("PickLatest: %v", err)
	}
	if got != want {
		t.Errorf("got %s, expected top-level file (no recursion)", got)
	}
}

func TestPickLatest_IgnoresZeroByte(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeFile(t, dir, "in-progress.zip", "", now) // 0 bytes — partial write
	want := writeFile(t, dir, "complete.zip", "real", now.Add(-1*time.Minute))

	got, _, err := PickLatest(dir)
	if err != nil {
		t.Fatalf("PickLatest: %v", err)
	}
	if got != want {
		t.Errorf("got %s, expected zero-byte file skipped", got)
	}
}

func TestPickLatest_EmptyDirReturnsErrNoFiles(t *testing.T) {
	dir := t.TempDir()
	_, _, err := PickLatest(dir)
	if !errors.Is(err, ErrNoFiles) {
		t.Errorf("empty dir: want ErrNoFiles, got %v", err)
	}
}

func TestPickLatest_MissingDirReturnsNotExist(t *testing.T) {
	_, _, err := PickLatest(filepath.Join(t.TempDir(), "nope"))
	if !os.IsNotExist(err) {
		t.Errorf("missing dir: want os.IsNotExist, got %v", err)
	}
}

func TestPickLatest_EmptyPathRejected(t *testing.T) {
	if _, _, err := PickLatest(""); err == nil {
		t.Error("empty path should error")
	}
}

func TestPickLatest_TieBreakLexicographic(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeFile(t, dir, "backup-b.zip", "b", now)
	want := writeFile(t, dir, "backup-a.zip", "a", now) // same mtime → 'a' wins

	got, _, err := PickLatest(dir)
	if err != nil {
		t.Fatalf("PickLatest: %v", err)
	}
	if got != want {
		t.Errorf("tie-break: got %s, want %s", got, want)
	}
}
