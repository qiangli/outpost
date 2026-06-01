//go:build windows

package sshclient

import "os"

// Windows has no SIGWINCH equivalent. Setting sigwinch to nil makes
// signal.Notify a no-op, so the SIGWINCH-forwarding goroutine never
// fires — appropriate behavior: a Windows terminal resize doesn't
// propagate to the remote PTY through this client.
var sigwinch os.Signal = nil
