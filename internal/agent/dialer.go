package agent

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/qiangli/outpost/internal/agent/localsock"
)

// dialSocket connects to a local socket so an http.Transport can speak
// HTTP over it. Scheme is "unix" (AF_UNIX; Linux, macOS, and Windows 10
// 1803+) or "npipe" (Windows named pipe; non-Windows builds error at
// request time).
//
// The scheme stays an EXPLICIT parameter rather than being inferred from
// the path, because it comes from conf.AppConfig.Scheme, which the
// operator sets by hand: an app misconfigured `npipe` on a Linux host
// must fail loudly, not get silently reinterpreted as a unix socket.
// The transport work itself lives in localsock, shared with the vknode
// libpod client and the sandbox pre-warmer so there is one named-pipe
// dial in the tree.
func dialSocket(ctx context.Context, scheme, socket string) (net.Conn, error) {
	switch strings.ToLower(scheme) {
	case localsock.SchemeUnix:
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	case localsock.SchemeNPipe:
		return localsock.Dial(ctx, localsock.PipePath(strings.TrimPrefix(socket, localsock.PipePrefix)))
	default:
		return nil, fmt.Errorf("dialSocket: unsupported scheme %q", scheme)
	}
}
