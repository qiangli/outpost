//go:build unix

package sshclient

import (
	"os"
	"syscall"
)

// sigwinch is the terminal-resize signal. On Unix it's SIGWINCH; on
// Windows there is no equivalent and the Shell loop skips the resize
// goroutine entirely (see sigwinch_windows.go).
var sigwinch os.Signal = syscall.SIGWINCH
