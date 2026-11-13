package agent

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
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

// TrustLoopback reports whether the escape hatch is set.
func (r *AppRegistry) TrustLoopback() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.trustLoopback
}

// tunnelOnly guards the routes the cloud drives (/shell, /desktop,
// /clipboard, upgrade/repair/warm): they carry no credential check of their
// own because cloudbox gates them upstream, so they are served only to
// requests that arrived through the tunnel ingress.
func tunnelOnly(apps *AppRegistry) gin.HandlerFunc {
	return func(c *gin.Context) {
		if ViaTunnelIngress(c.Request.Context()) {
			return
		}
		if !apps.TrustLoopback() {
			c.String(http.StatusForbidden, "tunnel only: use the outpost admin UI or the cloud tunnel\n")
			c.Abort()
			return
		}
		localOriginGuard(c)
	}
}

// localOriginGuard refuses a cross-origin browser request that did not come
// through the tunnel ingress. These WebSocket handlers accept without an
// origin check (cloudbox's proxy is the browser's origin), so off the ingress
// a web page in the owner's browser must not be able to reach them. Clients
// that send no Origin (ssh-proxy, CLIs) are unaffected.
func localOriginGuard(c *gin.Context) {
	if ViaTunnelIngress(c.Request.Context()) {
		return
	}
	origin := c.GetHeader("Origin")
	if origin == "" {
		return
	}
	// Same-origin only, and only for a loopback Host: under DNS rebinding
	// Origin and Host agree on the attacker's name, so a matching pair
	// alone proves nothing.
	if u, err := url.Parse(origin); err == nil && strings.EqualFold(u.Host, c.Request.Host) && loopbackHost(c.Request.Host) {
		return
	}
	c.String(http.StatusForbidden, "cross-origin request refused\n")
	c.Abort()
}

// loopbackHost reports whether a Host header names the loopback interface.
func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
