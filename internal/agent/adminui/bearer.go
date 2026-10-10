package adminui

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/hostauth"
)

const (
	ctxAuthVia    = "admin_auth_via"
	authViaBearer = "bearer"
)

// bearerIdentity admits a request that carries the host's own MCP bearer
// credential, the same secret `outpost` CLI subcommands read from the 0600
// config file. It exists so a same-host console (yoke webconsole's Host
// panel) can front this UI without a second password prompt.
//
// Every condition is mandatory, and a miss falls through to the cookie gate:
//
//   - the listener is loopback-only, and the TCP peer itself is loopback.
//     The raw RemoteAddr is used — gin's ClientIP honours X-Forwarded-For,
//     which a caller controls.
//   - the credential is a non-empty Bearer matching the live MCP token,
//     compared as fixed-length digests in constant time. A rotated token
//     stops working immediately because MCPToken reads the live value.
//   - the identity granted is the running OS user — the same single
//     principal /api/login admits — never a caller-supplied name.
func (s *Server) bearerIdentity(c *gin.Context) (string, bool) {
	if !s.loopbackOnly || s.deps.MCPToken == nil || !peerIsLoopback(c.Request.RemoteAddr) {
		return "", false
	}
	if !s.bearerMatches(c.Request) {
		return "", false
	}
	user, err := hostauth.CurrentUser()
	if err != nil || user == "" {
		return "", false
	}
	return user, true
}

func (s *Server) bearerMatches(r *http.Request) bool {
	if s.deps.MCPToken == nil {
		return false
	}
	want := s.deps.MCPToken()
	if want == "" {
		return false
	}
	const prefix = "Bearer "
	got := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(got, prefix) {
		return false
	}
	got = strings.TrimSpace(got[len(prefix):])
	if got == "" {
		return false
	}
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func peerIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// denyBearer refuses requests admitted by the host bearer on routes that
// reveal or replace a credential. The adapter must never turn the console's
// proxy into a way for a browser to read the secret the proxy holds.
func (s *Server) denyBearer() gin.HandlerFunc {
	return func(c *gin.Context) {
		if v, _ := c.Get(ctxAuthVia); v == authViaBearer {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "credential endpoints require an interactive login"})
			return
		}
		c.Next()
	}
}
