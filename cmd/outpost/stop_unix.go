//go:build !windows

package main

import (
	"os"
	"syscall"
)

// requestStop asks the daemon to exit gracefully. On unix that's a
// SIGTERM: main.go's `start` installs a signal.NotifyContext for it and
// drains all three listeners before returning, so the port is released
// cleanly.
func requestStop(proc *os.Process, _ int) error {
	return proc.Signal(syscall.SIGTERM)
}

// forceStop escalates when a graceful stop was ignored. SIGKILL cannot be
// caught, so the kernel reaps the process and frees the matrix-tunnel port
// unconditionally.
//
// These live in a build-tagged file because os.Process.Signal is WRONG on
// Windows: Signal returns ErrNotSupported (EWINDOWS) for every signal
// except Kill, so a SIGTERM stop returns an error without stopping
// anything — and the daemon keeps holding the fixed remote port, which is
// exactly the "restart orphans a stale frp session, host goes dark"
// failure. See stop_windows.go.
func forceStop(proc *os.Process, _ int) error {
	return proc.Signal(syscall.SIGKILL)
}
