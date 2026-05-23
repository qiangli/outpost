// Copyright (c) 2026, the outpost authors
// See LICENSE for licensing information

package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestJobRegistry_RecordList(t *testing.T) {
	dir := t.TempDir()
	r := NewJobRegistry(dir)

	// Use our own PID — guaranteed alive for the duration of the test.
	myPID := os.Getpid()
	if err := r.Record(myPID, "(detached)"); err != nil {
		t.Fatalf("Record: %v", err)
	}

	rows, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d, want 1", len(rows))
	}
	if rows[0].PID != myPID {
		t.Errorf("PID=%d, want %d", rows[0].PID, myPID)
	}
	if rows[0].Cmd != "(detached)" {
		t.Errorf("Cmd=%q, want %q", rows[0].Cmd, "(detached)")
	}
	if rows[0].User == "" {
		t.Errorf("User should be populated")
	}
	if rows[0].StartedAt.IsZero() {
		t.Errorf("StartedAt should be set")
	}
}

func TestJobRegistry_PrunesDeadPid(t *testing.T) {
	dir := t.TempDir()
	r := NewJobRegistry(dir)

	// Spawn + reap a real child so we have a kernel PID that's
	// guaranteed-gone (not just an arbitrary number that may belong to
	// something we don't own → EPERM, which pidAlive treats as alive).
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	deadPID := cmd.ProcessState.Pid()
	if err := r.Record(deadPID, "(was here)"); err != nil {
		t.Fatalf("Record: %v", err)
	}

	rows, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("dead pid not pruned, got rows=%+v", rows)
	}
	// File should be physically gone after the prune.
	if _, err := os.Stat(filepath.Join(dir, "1.json")); !os.IsNotExist(err) {
		// (file name uses the actual pid, not "1") — list above already
		// implies removal, this is a belt-and-braces check that ReadDir
		// won't find the stale record next time either.
		entries, _ := os.ReadDir(dir)
		if len(entries) > 0 {
			t.Errorf("dir not empty after prune: %v", entries)
		}
	}
}

func TestJobRegistry_DeleteIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	r := NewJobRegistry(dir)

	myPID := os.Getpid()
	if err := r.Record(myPID, "x"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := r.Delete(myPID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Second delete should not error — gives the CLI a stable contract
	// when racing with the daemon's own prune.
	if err := r.Delete(myPID); err != nil {
		t.Errorf("Delete (idempotent): %v", err)
	}
}

func TestJobRegistry_GetMissing(t *testing.T) {
	dir := t.TempDir()
	r := NewJobRegistry(dir)
	if _, err := r.Get(99999); err == nil {
		t.Error("Get on missing pid should error")
	}
}

func TestPidAlive(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Error("pidAlive(self) should be true")
	}
	// PID 0 is reserved; treat as dead per the function's contract.
	if pidAlive(0) {
		t.Error("pidAlive(0) should be false")
	}
}

func TestNoopRegistry(t *testing.T) {
	// Empty-dir registry is the no-op fallback used when UserCacheDir
	// is unavailable. Record errors politely, List returns empty.
	r := NewJobRegistry("")
	if err := r.Record(1234, "x"); err == nil {
		t.Error("no-op Record should error")
	}
	rows, err := r.List()
	if err != nil {
		t.Errorf("no-op List should not error: %v", err)
	}
	if rows != nil {
		t.Errorf("no-op List should return nil, got %+v", rows)
	}
	if err := r.Delete(1234); err != nil {
		t.Errorf("no-op Delete should not error: %v", err)
	}
}
