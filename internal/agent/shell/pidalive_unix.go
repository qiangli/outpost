//go:build !windows

package shell

import (
	"errors"
	"syscall"
)

// pidAlive — Unix semantics. syscall.Kill(pid, 0): nil = alive +
// owned by us; EPERM = alive but owned by another user (treat as
// alive — conservative); ESRCH = no such process.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH)
}
