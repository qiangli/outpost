//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach puts the child into its own session so closing the controlling
// terminal (or the parent's exit) won't propagate SIGHUP. This is the
// minimum a long-running daemon needs to survive `register; close laptop
// lid; reopen`.
func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}

// detachWithoutBreakaway exists only to satisfy the cross-platform spawn
// helper's fallback path. Breakaway-from-job is a Windows concept; on unix
// Setsid never fails for that reason, so there is nothing to fall back to.
// Returns false so the caller does NOT bother re-spawning.
func detachWithoutBreakaway(c *exec.Cmd) bool {
	c.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
	return false
}
