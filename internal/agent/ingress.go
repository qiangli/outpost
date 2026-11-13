package agent

import (
	"context"
	"net"
	"os"
	"path/filepath"
)

// The tunnel ingress is the only door through which cloud-arrived requests
// reach /app/<name>/*. It is a unix socket in a private (0700) directory that
// the matrix tunnel dials, so no other local process or user can connect to
// it. Requests served from it carry a context mark; the loopback/LAN listener
// serves the same engine without that mark.
//
// Why a separate door: on a shared listener every header the app gate reads
// (X-Forwarded-Prefix, X-Periscope-User, X-Periscope-Role) can be forged by a
// local process, so a login-required app has no trustworthy caller identity
// there. Apps that require login are therefore served only through the tunnel
// ingress; public (require_login=false) apps keep working on the local
// listener, which is where kiosk-style endpoints belong.

type tunnelIngressKey struct{}

// TunnelIngressConnContext marks every request on a connection accepted from
// the tunnel ingress listener. Use it as that http.Server's ConnContext.
func TunnelIngressConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, tunnelIngressKey{}, true)
}

// ViaTunnelIngress reports whether the request arrived through the tunnel
// ingress.
func ViaTunnelIngress(ctx context.Context) bool {
	v, _ := ctx.Value(tunnelIngressKey{}).(bool)
	return v
}

// TrustLoopbackEnv is the logged escape hatch: set to 1 to restore the old
// model (tunnel dials the loopback listener, loopback is trusted). Only for a
// host where the unix-socket ingress cannot be created.
const TrustLoopbackEnv = "OUTPOST_INGRESS_TRUST_LOOPBACK"

// ListenTunnelIngress creates a private directory and a unix socket in it.
// The returned cleanup closes nothing (the server owns the listener) but
// removes the directory.
func ListenTunnelIngress() (net.Listener, string, func(), error) {
	dir, err := os.MkdirTemp("", "outpost-ingress-")
	if err != nil {
		return nil, "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := os.Chmod(dir, 0o700); err != nil {
		cleanup()
		return nil, "", nil, err
	}
	path := filepath.Join(dir, "ingress.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		cleanup()
		return nil, "", nil, err
	}
	_ = os.Chmod(path, 0o600)
	return ln, path, cleanup, nil
}
