//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// Windows equivalents for "detach from the parent terminal".
// CREATE_NEW_PROCESS_GROUP + DETACHED_PROCESS — the new process gets no
// console of its own, and Ctrl-C in the parent terminal is no longer
// delivered to it. CREATE_BREAKAWAY_FROM_JOB — the new process starts
// OUTSIDE the parent's job object (see detach below for why that matters).
const (
	createNewProcessGroup  = 0x00000200
	detachedProcess        = 0x00000008
	createBreakawayFromJob = 0x01000000
)

// detach configures c to run as an independent background daemon: no console,
// its own process group, and — the Windows-critical part — BROKEN AWAY from the
// parent's job object.
//
// Why breakaway is load-bearing: Task Scheduler (how the outpost daemon is
// launched on Windows) runs its action inside a JOB OBJECT that, by default,
// terminates every process in the job when the task's main process exits. Our
// restart works by self-spawning — the running daemon launches a fresh copy of
// itself as a detached child and then exits. Without breakaway that child lands
// in the SAME job, so the instant the parent exits (which it now does reliably,
// via execSelfStart's os.Exit), Task Scheduler tears the job down and kills the
// child too — leaving NO daemon (the "failed start" that triggers an upgrade
// rollback). The flaky pre-fix behavior — a lingering DUPLICATE — was the same
// job-teardown race won the other way by the parent's slow stack-unwind exit.
// CREATE_BREAKAWAY_FROM_JOB starts the child as its own independent process so
// it survives the parent's exit. Fallback: detachWithoutBreakaway.
func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess | createBreakawayFromJob,
	}
}

// detachWithoutBreakaway is the fallback for when CreateProcess fails with the
// breakaway flag set: a job object created without JOB_OBJECT_LIMIT_BREAKAWAY_OK
// forbids breakaway, and CreateProcess then returns "access denied". Retrying
// without the flag keeps the child inside the job (so it's still vulnerable to
// job teardown), but a spawned-but-vulnerable daemon beats no daemon at all —
// and most Task Scheduler jobs DO permit breakaway, so this path is rare.
// Returns true so the caller knows a no-breakaway retry is worth attempting on
// Windows.
func detachWithoutBreakaway(c *exec.Cmd) bool {
	c.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess,
	}
	return true
}
