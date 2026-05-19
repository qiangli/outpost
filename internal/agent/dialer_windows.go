//go:build windows

package agent

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// dialNamedPipe dials a Windows named pipe (e.g. \\.\pipe\docker_engine).
// Used by socketTransport when scheme=npipe.
func dialNamedPipe(ctx context.Context, socket string) (net.Conn, error) {
	var timeout time.Duration
	if dl, ok := ctx.Deadline(); ok {
		timeout = time.Until(dl)
		if timeout <= 0 {
			return nil, fmt.Errorf("dialNamedPipe %q: deadline already passed", socket)
		}
	} else {
		timeout = 10 * time.Second
	}
	return winio.DialPipe(socket, &timeout)
}
