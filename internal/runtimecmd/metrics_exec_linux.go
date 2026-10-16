//go:build linux

package runtimecmd

import (
	"fmt"
	"os"
	"syscall"
)

// execMetrics drops privileges process-wide before replacing the launcher.
// syscall's credential setters update every Go runtime thread on Linux.
func execMetrics(args []string) error {
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("clear metrics groups: %w", err)
	}
	if err := syscall.Setgid(65532); err != nil {
		return fmt.Errorf("set metrics gid: %w", err)
	}
	if err := syscall.Setuid(65532); err != nil {
		return fmt.Errorf("set metrics uid: %w", err)
	}
	const binary = "/usr/local/bin/metrics-server"
	return syscall.Exec(binary, append([]string{binary}, args...), os.Environ())
}
