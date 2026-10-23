package main

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
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

// TestClaimPidFileIgnoresRecycledPid: after a reboot the daemon pidfile can
// name a pid now owned by an unrelated live process (observed on Windows:
// svchost). Liveness alone made claimPidFile refuse "already running"
// forever while the supervisor looped; the pid must also be an outpost.
func TestClaimPidFileIgnoresRecycledPid(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	bin, args := "sleep", []string{"30"}
	if runtime.GOOS == "windows" {
		bin, args = "ping", []string{"-n", "30", "127.0.0.1"}
	}
	cmd := exec.Command(bin, args...)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a long-lived helper: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	other := cmd.Process.Pid
	if !processAlive(other) {
		t.Skip("helper not observable as alive")
	}
	if daemonPidLive(other) {
		t.Fatalf("daemonPidLive(%d) = true for a non-outpost process (%s)", other, bin)
	}
	p, err := pidFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strconv.Itoa(other)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := claimPidFile(); err != nil {
		t.Fatalf("claimPidFile refused a pidfile naming a recycled non-outpost pid: %v", err)
	}
	got, _ := os.ReadFile(p)
	if strings.TrimSpace(string(got)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("pidfile = %q, want this process %d", got, os.Getpid())
	}
	// A live outpost (this test binary, outpost.test) still blocks a second claim.
	if err := claimPidFile(); err == nil {
		t.Fatal("claimPidFile accepted a pidfile naming a live outpost process")
	}
}
