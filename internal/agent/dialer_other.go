//go:build !windows

package agent

import (
	"context"
	"errors"
	"net"
)

// dialNamedPipe is a Windows-only operation; on every other platform we
// return a clear error so a misconfigured app fails loudly rather than
// silently hanging.
func dialNamedPipe(_ context.Context, _ string) (net.Conn, error) {
	return nil, errors.New("npipe scheme is only supported on Windows builds")
}
