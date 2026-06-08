package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRegisterRejectsEmpty(t *testing.T) {
	s := New("")
	if err := s.Register("", "@every 1m", func(ctx context.Context) error { return nil }); err == nil {
		t.Error("Register with empty name should error")
	}
	if err := s.Register("job", "@every 1m", nil); err == nil {
		t.Error("Register with nil JobFunc should error")
	}
}

func TestRegisterRejectsBadSpec(t *testing.T) {
	s := New("")
	if err := s.Register("job", "not a cron spec", func(ctx context.Context) error { return nil }); err == nil {
		t.Error("Register with malformed spec should error")
	}
}

func TestReRegisterReplaces(t *testing.T) {
	s := New("")
	for i, spec := range []string{"@every 1m", "@every 30s", "0 2 * * *"} {
		if err := s.Register("job", spec, func(ctx context.Context) error { return nil }); err != nil {
			t.Fatalf("Register #%d (%q): %v", i, spec, err)
		}
		names := s.Names()
		if len(names) != 1 || names[0] != "job" {
			t.Fatalf("after Register #%d: expected only [job], got %v", i, names)
		}
	}
}

func TestNamesAndRemove(t *testing.T) {
	s := New("")
	for _, n := range []string{"a", "b", "c"} {
		if err := s.Register(n, "@every 1h", func(ctx context.Context) error { return nil }); err != nil {
			t.Fatalf("Register %q: %v", n, err)
		}
	}
	if got := len(s.Names()); got != 3 {
		t.Fatalf("expected 3 names, got %d", got)
	}
	s.Remove("b")
	names := s.Names()
	if len(names) != 2 {
		t.Fatalf("expected 2 names after Remove, got %d (%v)", len(names), names)
	}
	for _, n := range names {
		if n == "b" {
			t.Errorf("b should have been removed")
		}
	}
	// Removing an unknown name is a no-op (concurrent dropped policy).
	s.Remove("nonexistent")
}

func TestNextRunAfterStart(t *testing.T) {
	s := New("")
	if err := s.Register("job", "@every 1h", func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()
	// Give the cron run-loop a moment to compute Entry.Next.
	time.Sleep(100 * time.Millisecond)
	next := s.NextRun("job")
	cancel()
	<-done
	if next.IsZero() {
		t.Error("NextRun after Run started should not be zero")
	}
	if !next.After(time.Now()) {
		t.Errorf("NextRun should be in the future, got %v (now %v)", next, time.Now())
	}
}

// Drive the wrap function directly to verify the ledger-write
// contract without waiting on cron's 1-second minimum tick.
func TestWrapWritesLedgerOK(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "scheduler.log"))
	called := false
	wrapped := s.wrap("manual", func(ctx context.Context) error {
		called = true
		return nil
	})
	wrapped()
	if !called {
		t.Fatal("wrapped fn was not called")
	}
	last, err := s.Ledger().LastByJob("manual")
	if err != nil {
		t.Fatalf("LastByJob: %v", err)
	}
	if last.Step != StepOK {
		t.Errorf("expected last step %q, got %q", StepOK, last.Step)
	}
}

func TestWrapWritesLedgerFailure(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "scheduler.log"))
	wrapped := s.wrap("flake", func(ctx context.Context) error {
		return errors.New("boom")
	})
	wrapped()
	last, err := s.Ledger().LastByJob("flake")
	if err != nil {
		t.Fatalf("LastByJob: %v", err)
	}
	if last.Step != StepFailed {
		t.Fatalf("expected last step %q, got %q", StepFailed, last.Step)
	}
	if last.Error == "" {
		t.Error("expected error message captured in ledger")
	}
	if last.DurationMs < 0 {
		t.Errorf("DurationMs should be non-negative, got %d", last.DurationMs)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	s := New("")
	if err := s.Register("job", "@every 1h", func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of ctx cancel")
	}
}

func TestLedgerAppendAndTail(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "l.log"))
	for i := 0; i < 5; i++ {
		if err := l.Append(LedgerEntry{Step: StepOK, Job: "j"}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	got, err := l.Tail(3)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries (last 3 of 5), got %d", len(got))
	}
	all, err := l.Tail(0)
	if err != nil {
		t.Fatalf("Tail(0): %v", err)
	}
	if len(all) != 5 {
		t.Errorf("Tail(0) should return all 5, got %d", len(all))
	}
}

func TestLedgerEmptyPathSilent(t *testing.T) {
	l := NewLedger("")
	if err := l.Append(LedgerEntry{Step: StepOK}); err != nil {
		t.Errorf("Append with empty path should be silent no-op, got %v", err)
	}
	got, err := l.Tail(10)
	if err != nil {
		t.Errorf("Tail with empty path should be silent no-op, got err %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Tail with empty path should return nothing, got %v", got)
	}
}

func TestLedgerTailMissingFileNotError(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "never-written.log"))
	got, err := l.Tail(10)
	if err != nil {
		t.Fatalf("Tail of missing file should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty tail, got %v", got)
	}
}

func TestLedgerLastByJobNoFireReturnsZero(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "l.log"))
	// Only a "fired" entry — LastByJob skips it because the job
	// outcome (ok/failed) hasn't been recorded yet.
	if err := l.Append(LedgerEntry{Step: StepFired, Job: "j"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	last, err := l.LastByJob("j")
	if err != nil {
		t.Fatalf("LastByJob: %v", err)
	}
	if last.Step != "" {
		t.Errorf("expected zero entry when only Fired exists, got %+v", last)
	}
}

func TestResolveLocationUsesTZ(t *testing.T) {
	t.Setenv("TZ", "UTC")
	loc := resolveLocation()
	if loc.String() != "UTC" {
		t.Errorf("expected UTC, got %v", loc)
	}
	t.Setenv("TZ", "")
	loc = resolveLocation()
	if loc == nil {
		t.Error("expected non-nil location (fallback to time.Local)")
	}
}

func TestNewReturnsUsableScheduler(t *testing.T) {
	s := New("")
	if s == nil {
		t.Fatal("New returned nil")
	}
	if s.Ledger() == nil {
		t.Fatal("Ledger should be non-nil even with empty path (no-op writes)")
	}
	if names := s.Names(); len(names) != 0 {
		t.Errorf("expected empty Names from fresh Scheduler, got %v", names)
	}
}
