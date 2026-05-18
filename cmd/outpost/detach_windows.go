//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// Windows equivalents for "detach from the parent terminal".
// CREATE_NEW_PROCESS_GROUP + DETACHED_PROCESS — the new process gets no
// console of its own, and Ctrl-C in the parent terminal is no longer
// delivered to it.
const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess,
	}
}
