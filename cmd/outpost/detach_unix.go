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
