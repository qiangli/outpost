//go:build !windows

package localsock

import (
	"context"
	"errors"
	"net"
	"os"
)

// dialPipe fails loudly off Windows. A named-pipe path reaching this
// build means a config or detection bug, and a silent nil conn would
// surface later as an unrelated HTTP error.
func dialPipe(_ context.Context, path string) (net.Conn, error) {
	return nil, errors.New("localsock: named pipes are only supported on Windows (got " + path + ")")
}

// statOK asserts the path really is a socket before Probe spends a dial.
// On unix the mode bit exists and is worth checking: it distinguishes a
// live socket from a leftover regular file of the same name.
func statOK(path string) bool {
	if IsPipe(path) {
		// Not reachable by a working config; let Dial return the clear
		// "pipes are Windows-only" error instead of a bare false.
		return true
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSocket != 0
}

// ListPipes has no meaning off Windows.
func ListPipes(string) []string { return nil }
