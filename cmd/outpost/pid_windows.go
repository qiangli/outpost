//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// stillActive is GetExitCodeProcess's STILL_ACTIVE sentinel: a process
// that has not yet exited reports this as its exit code.
const stillActive = 259

// processAlive reports whether pid names a live process.
//
// This exists because the obvious portable form — os.FindProcess then
// Signal(0) — is silently broken on Windows: os.Process.Signal returns
// EWINDOWS for every signal except Kill, so the probe reports a LIVE
// process as dead. That made the daemon's single-instance guard a no-op
// on Windows: claimPidFile read a pidfile pointing at a healthy daemon,
// asked processAlive, was told "not running", and started a SECOND
// daemon. The two then fought over the fixed matrix-tunnel remote port,
// and the loser's stale frp session left the host dark — a restart or
// self-upgrade was enough to trigger it.
//
// OpenProcess + GetExitCodeProcess is the correct query, and mirrors the
// implementation already used by the vknode native-process backend.
// PROCESS_QUERY_LIMITED_INFORMATION is deliberately the narrowest right
// that answers the question.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// processName identifies a live process before trusting a stale supervisor
// pidfile. A recycled PID must not keep the real supervisor from starting.
func processName(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	name := make([]uint16, 32768)
	size := uint32(len(name))
	if err := windows.QueryFullProcessImageName(h, 0, &name[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(name[:size]), nil
}

// processArgs is not available on Windows; the init handoff there is
// warn-only and never signals, so it needs no command line.
func processArgs(int) (string, error) {
	return "", errors.New("process command line not available on windows")
}

// listProcesses is not available on Windows; discovery there is pidfile-only.
func listProcesses() ([]procEntry, error) {
	return nil, errors.New("process table scan not available on windows")
}
