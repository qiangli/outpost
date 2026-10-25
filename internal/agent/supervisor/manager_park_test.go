package supervisor

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestMain intercepts the helper-process mode: when the supervisor spawns
// the test binary as a fake child (SUPERVISOR_TEST_CHILD=1), exit with the
// code from SUPERVISOR_TEST_EXIT instead of running the suite. This is the
// cross-platform "fake process" — no /bin/sh, so these tests run on
// Windows too.
func TestMain(m *testing.M) {
	if os.Getenv("SUPERVISOR_TEST_CHILD") == "1" {
		code := 0
		if n, err := strconv.Atoi(os.Getenv("SUPERVISOR_TEST_EXIT")); err == nil {
			code = n
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// childProgram returns a Program that re-execs the test binary as a fake
// child exiting immediately with the given code.
func childProgram(name string, exit int, gate func() bool) *Program {
	return &Program{
		Name:       name,
		Path:       os.Args[0],
		Env:        append(os.Environ(), "SUPERVISOR_TEST_CHILD=1", "SUPERVISOR_TEST_EXIT="+strconv.Itoa(exit)),
		ShouldRun:  gate,
		MinBackoff: 10 * time.Millisecond,
		MaxBackoff: 20 * time.Millisecond,
		StartSecs:  time.Hour, // never "healthy" → every exit is a fast crash
	}
}

func runManager(t *testing.T, m *Manager, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(d + 5*time.Second):
		t.Fatal("Run did not return after ctx timeout")
	}
}

// TestManager_RestartsAfterUnexpectedExit: a child that exits on its own
// (the kill -9 / crash case — no stop intent was written) is restarted
// with backoff, so its start count climbs.
func TestManager_RestartsAfterUnexpectedExit(t *testing.T) {
	m := New(childProgram("blip", 1, nil))
	runManager(t, m, 500*time.Millisecond)
	if got := m.Snapshot()[0].Starts; got < 2 {
		t.Fatalf("starts = %d, want >= 2 (crashed child should have restarted)", got)
	}
	if m.Snapshot()[0].Parked {
		t.Fatal("parked = true after crash restarts; nothing wrote a stop intent")
	}
}

// TestManager_RestartsAfterCleanExit: even an exit 0 restarts while no
// stop/disable intent parks the program — only the intent holds it down.
func TestManager_RestartsAfterCleanExit(t *testing.T) {
	m := New(childProgram("quitter", 0, nil))
	runManager(t, m, 500*time.Millisecond)
	if got := m.Snapshot()[0].Starts; got < 2 {
		t.Fatalf("starts = %d, want >= 2 (clean exit must still restart without intent)", got)
	}
}

// TestManager_ParkedProgramNeverStarts: while the ShouldRun gate reports
// false (a stop/disable intent is on disk), the supervisor launches
// nothing — the stop case, not the kill case.
func TestManager_ParkedProgramNeverStarts(t *testing.T) {
	m := New(childProgram("held", 1, func() bool { return false }))
	runManager(t, m, 300*time.Millisecond)
	snap := m.Snapshot()[0]
	if snap.Starts != 0 {
		t.Fatalf("starts = %d, want 0 (parked program must never launch)", snap.Starts)
	}
	if !snap.Parked {
		t.Fatal("parked = false while the intent gate holds; want true")
	}
}

// TestManager_UnparkRelaunches: clearing the intent resumes launches —
// the `outpost enable` / boot-clear path.
func TestManager_UnparkRelaunches(t *testing.T) {
	running := false
	m := New(childProgram("resumed", 1, func() bool { return running }))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	time.Sleep(150 * time.Millisecond) // parked beats, no launches
	if got := m.Snapshot()[0].Starts; got != 0 {
		t.Fatalf("starts = %d before unpark, want 0", got)
	}
	running = true // clear the intent
	deadline := time.After(1500 * time.Millisecond)
	for {
		snap := m.Snapshot()[0]
		if snap.Starts >= 1 && !snap.Parked {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("still parked/never started after intent cleared: %+v", snap)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
