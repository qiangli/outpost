//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// requestStop / forceStop terminate the daemon on Windows.
//
// Unix signals do not exist here: os.Process.Signal returns
// ErrNotSupported for everything except Kill, so the portable-looking
// proc.Signal(syscall.SIGTERM) in stop_unix.go would fail WITHOUT stopping
// the daemon — and a daemon that can't be stopped can't be restarted,
// because it keeps holding the fixed matrix-tunnel remote port. That is
// the exact signature of the "restart orphans a stale frp session, host
// goes dark with 'port unavailable'" failure. pid_windows.go already
// documents the same Signal-is-broken-on-Windows quirk for the liveness
// probe; the stop path gets the matching treatment here.
//
// Windows has no in-band graceful-shutdown signal deliverable to a
// non-console-attached service process, so both the "graceful" and the
// "escalated" call map to TerminateProcess. We terminate strictly BY PID
// (via OpenProcess), never by image name — taskkill /IM would also kill
// the supervisor that restarts the daemon and auto-reverts a bad binary.
func requestStop(_ *os.Process, pid int) error {
	return terminateProcess(pid)
}

func forceStop(_ *os.Process, pid int) error {
	return terminateProcess(pid)
}

// terminateProcess opens the target with the narrowest right that lets us
// stop it (PROCESS_TERMINATE) and calls TerminateProcess. Exit code 1
// matches what os.Process.Kill uses on Windows.
func terminateProcess(pid int) error {
	if pid <= 0 {
		return os.ErrInvalid
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}
