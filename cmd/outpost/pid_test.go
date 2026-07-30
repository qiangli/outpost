package main

import (
	"os"
	"os/exec"
	"testing"
)

// TestProcessAliveSelf is the regression guard for a bug that was silent
// and expensive: on Windows the portable os.Process.Signal(0) probe
// returns EWINDOWS for a LIVE process, so processAlive reported every
// daemon as dead. claimPidFile then treated a healthy daemon's pidfile as
// stale and started a second instance; the two fought over the fixed
// matrix-tunnel remote port and left the host dark.
//
// Asserting on our own pid is the strongest available check: this process
// is definitionally alive on every platform, so a platform whose
// implementation is broken fails here rather than in production.
func TestProcessAliveSelf(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("processAlive(self) = false; the single-instance guard would be a no-op on this platform")
	}
}

// TestProcessAliveRejectsNonsense covers the guard clause: a
// non-positive pid must never be reported alive, or a truncated/garbage
// pidfile would look like a running daemon and block startup forever.
func TestProcessAliveRejectsNonsense(t *testing.T) {
	for _, pid := range []int{0, -1, -12345} {
		if processAlive(pid) {
			t.Errorf("processAlive(%d) = true, want false", pid)
		}
	}
}

// TestProcessAliveExitedProcess pins the other direction: a process that
// has genuinely exited must read as dead, so a stale pidfile from a crash
// does not permanently wedge startup. Uses a real spawned child that we
// reap, rather than guessing an unused pid — pid reuse would make that
// flaky.
func TestProcessAliveExitedProcess(t *testing.T) {
	cmd := exec.Command(exitZeroBin(), exitZeroArgs()...)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a helper on this platform: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Skipf("helper did not exit cleanly: %v", err)
	}
	if processAlive(pid) {
		t.Errorf("processAlive(%d) = true for a reaped process; a stale pidfile would block startup", pid)
	}
}
