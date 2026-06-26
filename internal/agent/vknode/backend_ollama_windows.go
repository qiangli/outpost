//go:build windows

package vknode

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

// stillActive is the GetExitCodeProcess sentinel for a process that has
// not yet exited (STILL_ACTIVE).
const stillActive = 259

// defaultLaunch starts a detached child in a NEW process group so it
// isn't taken down by a Ctrl-C delivered to the daemon, and so
// taskkill /T can later find the whole tree. stdout+stderr go to the
// per-pod log file. We reap with a background Wait.
//
// ctx is intentionally not tied to the process lifetime: the workload
// must outlive the Ensure call that launched it.
func defaultLaunch(_ context.Context, spec launchSpec) (int, error) {
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.Dir
	if spec.LogPath != "" {
		if f, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			cmd.Stdout = f
			cmd.Stderr = f
			defer f.Close()
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	return pid, nil
}

// processAlive reports whether pid names a live process by querying its
// exit code — STILL_ACTIVE means running. A handle we can't open is
// treated as gone.
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

// killProcessTree terminates the process tree rooted at pid. taskkill
// /T walks child processes (the ggml/rpc helpers ollama spawns); /F
// forces them. An "process not found" exit is treated as success.
func killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	// taskkill returns non-zero when the process is already gone; that's
	// not an error for our idempotent Delete contract.
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
	return nil
}
