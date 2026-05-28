//go:build !windows

package ycode

import (
	"os/exec"
	"syscall"
)

// platformDetach configures cmd so the spawned process becomes its
// own session leader. setsid (Setsid=true) detaches from the
// terminal; Setpgid + Pgid=0 puts the child in its own process
// group so SIGINT to outpost's process group doesn't propagate.
// Combined: the spawned ycode survives outpost exit cleanly.
func platformDetach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setpgid: true,
		Pgid:    0,
	}
}

// startDetached delegates to the swappable spawn variable so tests
// can intercept without going through exec.
func startDetached(bin string) error {
	return spawn(bin)
}
