//go:build !windows

package vknode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// defaultLaunch starts a detached child process in its own process
// group (Setpgid) so killProcessTree can later signal the whole tree —
// llama.cpp / ollama spawn helper workers (rpc-server, ggml backends)
// that must die with the parent. The child's stdout+stderr go to a
// per-pod log file under the data dir. We reap with a background Wait so
// a self-exiting child becomes a clean ESRCH for processAlive rather
// than a lingering zombie.
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
			// The child dups these fds at fork; close our copy after
			// Start so the file isn't held open by this process forever.
			defer f.Close()
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
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

// processAlive reports whether pid names a live process. Signal 0 is the
// canonical "does this process exist" probe — nil means alive, EPERM
// means alive-but-not-ours, ESRCH means gone.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// killProcessTree terminates the process group led by pid (we launch
// with Setpgid, so -pid addresses the whole tree). SIGTERM first for a
// graceful shutdown, then SIGKILL to guarantee the slot frees. A
// process that already exited (ESRCH) is success.
func killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	grp := -pid
	_ = syscall.Kill(grp, syscall.SIGTERM)
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Kill(grp, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		// The group signal failed (e.g. the child raced to exit before
		// it became a group leader) — fall back to the bare pid.
		if perr := syscall.Kill(pid, syscall.SIGKILL); perr != nil && !errors.Is(perr, syscall.ESRCH) {
			return perr
		}
	}
	return nil
}
