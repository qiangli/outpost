//go:build windows

package vknode

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

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
	helper, descriptor, err := prepareNativeHelper(spec)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(helper, nativeExecHelperArg, descriptor)
	cmd.Env = spec.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS | windows.CREATE_BREAKAWAY_FROM_JOB}
	if err := cmd.Start(); err != nil {
		cmd = exec.Command(helper, nativeExecHelperArg, descriptor)
		cmd.Env = spec.Env
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
		if retryErr := cmd.Start(); retryErr != nil {
			_ = os.Remove(descriptor)
			return 0, retryErr
		}
	}
	pid := cmd.Process.Pid
	go func() {
		err := cmd.Wait()
		if spec.OnExit != nil {
			spec.OnExit(pid, exitCodeFromWait(err, cmd.ProcessState), time.Now())
		}
	}()
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
