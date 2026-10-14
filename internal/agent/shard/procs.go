package shard

import (
	"os"
	"sync"
)

// killPID terminates a process by PID. A variable (not os.FindProcess inline)
// so hermetic tests record the exact PIDs this outpost would signal without
// touching real processes.
var killPID = func(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// procTracker is the exact set of prima processes THIS outpost started,
// by PID. Teardown and re-provisioning kill from this set only — never a
// pattern scan (pgrep/pkill by name or path), which could signal a sibling
// outpost, a developer's shell, or the daemon itself.
type procTracker struct {
	mu    sync.Mutex
	procs map[int]string // pid → prima binary path it was launched from
}

// trackedPrima is the process-wide registry: the shard manager (Form/Stop)
// and the provision path share it, so a (re)provision kills orphans even
// when the session that launched them is gone.
var trackedPrima = &procTracker{procs: map[int]string{}}

// TrackPrimaProc records a prima process this outpost started. Called by
// Start right after the process launches.
func TrackPrimaProc(pid int, binPath string) {
	if pid <= 0 || binPath == "" {
		return
	}
	trackedPrima.mu.Lock()
	defer trackedPrima.mu.Unlock()
	trackedPrima.procs[pid] = binPath
}

// UntrackPrimaProc forgets a prima process — it exited (or was reaped) so
// its PID must never be signaled again. PID reuse makes a stale entry
// dangerous: the number may later belong to an unrelated process.
func UntrackPrimaProc(pid int) {
	trackedPrima.mu.Lock()
	defer trackedPrima.mu.Unlock()
	delete(trackedPrima.procs, pid)
}

// TrackedPrimaPIDs returns the tracked PIDs launched from binPath.
func TrackedPrimaPIDs(binPath string) []int {
	if binPath == "" {
		return nil
	}
	trackedPrima.mu.Lock()
	defer trackedPrima.mu.Unlock()
	var out []int
	for pid, bin := range trackedPrima.procs {
		if bin == binPath {
			out = append(out, pid)
		}
	}
	return out
}

// KillTrackedPrima terminates the exact orphaned prima processes this
// outpost started from binPath (tracked PIDs only) and forgets them. It
// returns the PIDs successfully signaled. Entries are forgotten even when
// the signal fails — a kill error means the process is already gone (or
// unkillable), and either way the PID must not be signaled again.
func KillTrackedPrima(binPath string) []int {
	var killed []int
	for _, pid := range TrackedPrimaPIDs(binPath) {
		if killPID(pid) == nil {
			killed = append(killed, pid)
		}
		UntrackPrimaProc(pid)
	}
	return killed
}
