//go:build windows

package main

import "golang.org/x/sys/windows"

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
