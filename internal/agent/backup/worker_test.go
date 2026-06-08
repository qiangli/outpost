package backup

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestWorker_RunOnceProducesCandidate(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeFile(t, dir, "backup-1.zip", "payload one", now)

	ledger := NewLedger(filepath.Join(t.TempDir(), "backup.log"))
	w := NewWorker(ledger)
	out, err := w.RunOnce(context.Background(), []string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(out))
	}
	c := out[0]
	if c.Folder != dir {
		t.Errorf("folder %s, want %s", c.Folder, dir)
	}
	if c.SHA256 == "" {
		t.Error("SHA256 should be set")
	}
	if c.Size != int64(len("payload one")) {
		t.Errorf("size %d, want %d", c.Size, len("payload one"))
	}
	if c.Skipped {
		t.Error("first fire should not be Skipped")
	}
	if c.Error != "" {
		t.Errorf("unexpected error: %q", c.Error)
	}
}

func TestWorker_DedupBySHA(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "backup.zip", "same content", time.Now())

	ledger := NewLedger(filepath.Join(t.TempDir(), "backup.log"))
	w := NewWorker(ledger)

	// Two successive fires against the same unchanged file.
	first, err := w.RunOnce(context.Background(), []string{dir})
	if err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if first[0].Skipped {
		t.Fatal("first fire must not be Skipped")
	}
	second, err := w.RunOnce(context.Background(), []string{dir})
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if !second[0].Skipped {
		t.Errorf("second fire should be Skipped (same sha), got %+v", second[0])
	}
	if second[0].SHA256 != first[0].SHA256 {
		t.Errorf("SHAs should match: %s vs %s", first[0].SHA256, second[0].SHA256)
	}
}

func TestWorker_MultipleFoldersIndependent(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	writeFile(t, dirA, "a.zip", "alpha", time.Now())
	writeFile(t, dirB, "b.zip", "beta", time.Now())

	w := NewWorker(NewLedger(filepath.Join(t.TempDir(), "l.log")))
	out, err := w.RunOnce(context.Background(), []string{dirA, dirB})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(out))
	}
	if out[0].SHA256 == out[1].SHA256 {
		t.Error("different folders with different contents should produce different SHAs")
	}
}

func TestWorker_BadFolderDoesNotAbortOthers(t *testing.T) {
	good := t.TempDir()
	writeFile(t, good, "ok.zip", "ok", time.Now())

	w := NewWorker(NewLedger(""))
	out, err := w.RunOnce(context.Background(), []string{"/no/such/dir", good})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 candidates (one error + one ok), got %d", len(out))
	}
	if out[0].Error == "" {
		t.Errorf("first candidate should have Error, got %+v", out[0])
	}
	if out[1].Error != "" {
		t.Errorf("second candidate should have no Error, got %+v", out[1])
	}
	if out[1].SHA256 == "" {
		t.Error("good folder should have produced a SHA")
	}
}

func TestWorker_EmptyFolderRecordsError(t *testing.T) {
	dir := t.TempDir() // no files inside
	w := NewWorker(NewLedger(""))
	out, _ := w.RunOnce(context.Background(), []string{dir})
	if len(out) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(out))
	}
	if out[0].Error == "" {
		t.Errorf("empty folder should record an Error")
	}
	if out[0].SHA256 != "" {
		t.Errorf("empty folder should NOT have a SHA, got %q", out[0].SHA256)
	}
}

func TestWorker_InFlightRejection(t *testing.T) {
	w := NewWorker(NewLedger(""))
	// Manually set inFlight to simulate a concurrent run.
	w.mu.Lock()
	w.inFlight = true
	w.mu.Unlock()

	if _, err := w.RunOnce(context.Background(), []string{t.TempDir()}); err == nil {
		t.Error("expected in-flight rejection error")
	}
}

func TestLedger_LastByFolderSkipsSkipped(t *testing.T) {
	ledger := NewLedger(filepath.Join(t.TempDir(), "l.log"))
	folder := "/some/dir"
	// First: real candidate.
	if err := ledger.Append(Candidate{Folder: folder, SHA256: "abc", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// Second: skipped (dedup).
	if err := ledger.Append(Candidate{Folder: folder, SHA256: "abc", Skipped: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	got, err := ledger.LastByFolder(folder)
	if err != nil {
		t.Fatalf("LastByFolder: %v", err)
	}
	if got.SHA256 != "abc" {
		t.Errorf("want sha=abc, got %s", got.SHA256)
	}
	if got.Skipped {
		t.Errorf("LastByFolder should skip Skipped entries, got %+v", got)
	}
}

func TestLedger_LastByFolderNoneReturnsZero(t *testing.T) {
	ledger := NewLedger(filepath.Join(t.TempDir(), "l.log"))
	got, err := ledger.LastByFolder("nope")
	if err != nil {
		t.Fatalf("LastByFolder: %v", err)
	}
	if got.Folder != "" || got.SHA256 != "" {
		t.Errorf("expected zero candidate, got %+v", got)
	}
}
