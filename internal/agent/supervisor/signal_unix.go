//go:build !windows

package supervisor

import (
	"os"
	"syscall"
)

// stopSignal asks the child to terminate gracefully (SIGTERM); the os/exec
// WaitDelay then force-kills it if it doesn't exit within the grace window.
func stopSignal(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Signal(syscall.SIGTERM)
}
