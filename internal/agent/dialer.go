package agent

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// dialSocket connects to a local socket so an http.Transport can speak
// HTTP over it. Scheme is "unix" (AF_UNIX; Linux, macOS, and Windows 10
// 1803+) or "npipe" (Windows named pipe; non-Windows builds error at
// request time — see dialNamedPipe in dialer_{windows,other}.go).
func dialSocket(ctx context.Context, scheme, socket string) (net.Conn, error) {
	switch strings.ToLower(scheme) {
	case "unix":
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	case "npipe":
		return dialNamedPipe(ctx, socket)
	default:
		return nil, fmt.Errorf("dialSocket: unsupported scheme %q", scheme)
	}
}
