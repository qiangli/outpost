//go:build windows

package shell

import "golang.org/x/sys/windows"

// pidAlive — Windows variant. Opens the process with the minimal
// PROCESS_QUERY_LIMITED_INFORMATION right (allowed for all users on
// modern Windows even without elevation) and reads GetExitCodeProcess.
// STILL_ACTIVE (259) means the process hasn't exited yet. Any error
// opening the handle — including ERROR_INVALID_PARAMETER for a PID
// that doesn't exist — is treated as "not alive."
func pidAlive(pid int) bool {
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
	const stillActive = 259
	return code == stillActive
}
