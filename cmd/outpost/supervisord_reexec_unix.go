//go:build !windows

package main

import (
	"os"
	"syscall"
)

// reexecInitImpl execs the binary now at initSelf in place of this process.
// The pid survives, so the OS service registration and the pidfile (which
// claimSupervisordPidFile accepts as our own) stay valid.
func reexecInitImpl() error {
	return syscall.Exec(initSelf, os.Args, os.Environ())
}
