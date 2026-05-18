// Package agent runs on the home host, dials the cloud over frp, and
// exposes local apps (ycode, shell, desktop, plus user-defined LAN services).
package agent

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/hostauth"
)

// Deps is what `matrix-agent` (or a future host application) supplies to
// the agent's local HTTP routes.
//
// AuthURL switches /auth between two strategies:
//   - empty  → host-OS path. Submitted username must match the agent's
//     own OS user; password is verified via hostauth (PAM / dscl /
//     LogonUserW). Role defaults to admin; Admins, when non-empty,
//     downgrades emails not in the list to user.
//   - set    → delegate to an external endpoint. The agent POSTs
//     {user,password} and trusts the returned {user,role}. Admins is
//     ignored on this path.
type Deps struct {
	AgentName string
	Apps      *AppRegistry
	Auth      hostauth.Authenticator
	Admins    AdminSet
	AuthURL   string
	VNCAddr   string // default 127.0.0.1:5900
}

// RegisterRoutes attaches all matrix-agent routes onto rg. Always mounted
// at the root in the standalone binary; the routes are loopback-only and
// reached from the cloud through the frp tunnel.
func RegisterRoutes(rg *gin.RouterGroup, deps Deps) {
	rg.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	apps := deps.Apps
	if apps == nil {
		apps = NewAppRegistry()
	}
	auth := deps.Auth
	if auth == nil {
		auth = hostauth.DefaultAuthenticator()
	}

	rg.GET("/apps", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"agent": deps.AgentName, "apps": apps.Names()})
	})

	// Credential check (cloud's /h/:host/elevate proxies here). When
	// deps.AuthURL is set the agent delegates to it; otherwise it falls
	// back to OS auth via hostauth. See Deps doc for the full contract.
	rg.POST("/auth", authHandler(auth, deps.Admins, deps.AuthURL))

	// Tier-3 interactive shell. WS upgrade; the cloud also gates on
	// RequireElevation before proxying.
	rg.GET("/shell", shellHandler())

	// Tier-3 remote desktop. Binary WS ↔ TCP 5900.
	rg.GET("/desktop", desktopHandler(deps.VNCAddr))

	// Tier-3 clipboard bridge. GET returns the host's clipboard text
	// (pbpaste on macOS), POST replaces it (pbcopy). Bypasses RFB
	// clipboard so it works on plain-HTTP origins and works around
	// macOS Screen Sharing's non-standard clipboard protocol.
	rg.GET("/clipboard", clipboardHandler())
	rg.POST("/clipboard", clipboardHandler())

	// Reverse-proxy every method (GET/POST/WS upgrades included).
	rg.Any("/app/:name", apps.handler())
	rg.Any("/app/:name/*p", apps.handler())
}
