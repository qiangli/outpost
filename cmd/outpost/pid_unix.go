//go:build !windows

package main

import (
	"os"
	"syscall"
)

// processAlive reports whether pid names a live process. Signal 0 is
// POSIX's "could I deliver a signal" no-op.
//
// This lives in a build-tagged file because the portable-looking
// os.Process.Signal form is WRONG on Windows: there, Signal returns
// EWINDOWS for anything but Kill, so a Signal(0) probe reports every
// process — including a live one — as dead. See pid_windows.go.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
