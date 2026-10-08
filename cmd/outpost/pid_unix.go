//go:build !windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
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

// processName returns the executable name of pid as ps reports it.
func processName(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
