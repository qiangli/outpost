// Package agent runs on the home host, dials cloudbox over the matrix
// tunnel, and exposes local apps (ycode, shell, desktop, plus
// user-defined LAN services).
package agent

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh"

	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/outpost/internal/agent/peerhosts"
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

	// Built-in route toggles. Zero value = enabled, so callers that don't
	// care about toggling keep the old default-on behavior.
	ShellDisabled     bool
	DesktopDisabled   bool
	ClipboardDisabled bool
	SSHDisabled       bool

	// SSHAllowLocalForward gates whether the SSH server accepts
	// `direct-tcpip` channels (stock `ssh -L` / `ssh -D`). Zero value
	// (false) means rejected — callers must opt in. main.go threads
	// `fc.SSHAllowLocalForwardOn()` here, which defaults to true on
	// fresh + legacy configs.
	SSHAllowLocalForward bool

	// SSHAllowRemoteForward gates whether the SSH server honors
	// `tcpip-forward` global requests (stock `ssh -R`). Same opt-in /
	// loopback-bind story as SSHAllowLocalForward.
	SSHAllowRemoteForward bool

	// SSHAllowAgentForward gates whether the SSH server accepts
	// `auth-agent-req@openssh.com` channel-requests (stock `ssh -A`).
	// Default off here; main.go threads `fc.SSHAllowAgentForwardOn()`
	// which is default-on. Per-session Unix socket lives in a 0700
	// tempdir, set as SSH_AUTH_SOCK in the runner env.
	SSHAllowAgentForward bool

	// SFTPEnabled gates whether the SSH server accepts the "sftp"
	// subsystem request — required for modern openssh `scp` (8.8+) and
	// for `sftp` itself. Zero value (false) means rejected; callers must
	// opt in. Disabling forces clients to use legacy `scp -O` (the exec
	// channel) which is also supported but slower.
	SFTPEnabled bool

	// SSHHostKey is the persistent host identity for the embedded SSH
	// server reached at /ssh. Nil means /ssh will not mount even if
	// SSHDisabled is false — callers pass a key loaded via
	// LoadOrCreateHostKey() at boot.
	SSHHostKey ssh.Signer

	// PeerHosts widens the SSH `direct-tcpip` destination allowlist to
	// any hostname registered as a paired outpost in this cloudbox
	// account, on top of the always-allowed loopback set. Nil → only
	// loopback destinations (the pre-existing posture). Constructed in
	// main.go from fc.AccessToken + cloudbox endpoint, so unpaired
	// outposts pass nil and keep the tight default.
	PeerHosts *peerhosts.Registry

	// SSHForwardSockets extends the unix-socket allowlist for
	// `direct-streamlocal@openssh.com` channel-opens — the primitive
	// behind `podman --connection=<host>`. The built-in defaults
	// (podman + canonical docker sockets) always apply on top of this
	// list; entries here are exact-matched after filepath.Clean.
	SSHForwardSockets []string

	// SelfName is the agent's own AgentName, forwarded to the SSH
	// handler so peer-tunneled dials can stamp X-Outpost-Peer-Origin
	// for cloudbox's audit log. Empty string is harmless — cloudbox
	// just records "unknown" as the origin.
	SelfName string

	// CloudboxBase + CloudboxProtocol + AccessToken jointly enable the
	// peer-tunneled direct-tcpip path (`ssh -J peerA peerB`). When set
	// and the dial target is a paired peer (not loopback) on port 22,
	// the SSH server routes the bytes through cloudbox's
	// /h/<peerB>/ssh WSS endpoint instead of attempting a LAN net.Dial
	// that would usually fail on DNS. Empty fields keep the dial path
	// loopback-only-plus-LAN-DNS.
	CloudboxBase     string
	CloudboxProtocol string
	AccessToken      string
}

// RegisterRoutes attaches all matrix-agent routes onto rg. Always mounted
// at the root in the standalone binary; the routes are loopback-only and
// reached from cloudbox through the matrix tunnel.
func RegisterRoutes(rg *gin.RouterGroup, deps Deps) {
	rg.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	// Build provenance is captured once at startup — the binary's identity
	// doesn't change at runtime.
	build := ReadBuildInfo()
	rg.GET("/version", func(c *gin.Context) {
		c.JSON(http.StatusOK, build)
	})

	apps := deps.Apps
	if apps == nil {
		apps = NewAppRegistry()
	}
	auth := deps.Auth
	if auth == nil {
		auth = hostauth.DefaultAuthenticator()
	}

	rg.GET("/apps", func(c *gin.Context) {
		// New shape: [{name, role}] plus a `builtins` map so cloudbox knows
		// which of /shell, /desktop, /clipboard this agent actually serves.
		// `version` is the short commit (e.g. "06d8d89" or "06d8d89-dirty")
		// so cloudbox's /api/v1/hosts can show "outpost stale?" without
		// hitting a second endpoint. Older outposts omit `builtins` and
		// `version`; the cloud treats those as legacy / unknown.
		c.JSON(http.StatusOK, gin.H{
			"agent":   deps.AgentName,
			"version": build.Short(),
			"apps":    apps.Entries(),
			"builtins": gin.H{
				"shell":     !deps.ShellDisabled,
				"desktop":   !deps.DesktopDisabled,
				"clipboard": !deps.ClipboardDisabled,
				"ssh":       !deps.SSHDisabled && deps.SSHHostKey != nil,
				"sftp":      !deps.SSHDisabled && deps.SSHHostKey != nil && deps.SFTPEnabled,
			},
		})
	})

	// Credential check (cloud's /h/:host/elevate proxies here). When
	// deps.AuthURL is set the agent delegates to it; otherwise it falls
	// back to OS auth via hostauth. See Deps doc for the full contract.
	rg.POST("/auth", authHandler(auth, deps.Admins, deps.AuthURL))

	// Tier-3 interactive shell. WS upgrade; the cloud also gates on
	// RequireElevation before proxying. Disabled via the admin UI.
	if !deps.ShellDisabled {
		rg.GET("/shell", shellHandler())
	}

	// Tier-3 remote desktop. Binary WS ↔ TCP 5900.
	if !deps.DesktopDisabled {
		rg.GET("/desktop", desktopHandler(deps.VNCAddr))
	}

	// Tier-3 clipboard bridge. GET returns the host's clipboard text
	// (pbpaste on macOS), POST replaces it (pbcopy). Bypasses RFB
	// clipboard so it works on plain-HTTP origins and works around
	// macOS Screen Sharing's non-standard clipboard protocol.
	if !deps.ClipboardDisabled {
		rg.GET("/clipboard", clipboardHandler())
		rg.POST("/clipboard", clipboardHandler())
	}

	// Real SSH endpoint reached over WebSocket through the matrix tunnel.
	// Unlike /shell (browser PTY wired to the in-process qiangli/sh) this
	// is an actual SSH server: clients use standard `ssh`/`scp`/`rsync`
	// via the local `outpost ssh-proxy` ProxyCommand helper. Auth gate is
	// the OS password (same hostauth path as /auth).
	if !deps.SSHDisabled && deps.SSHHostKey != nil {
		rg.GET("/ssh", sshHandler(sshHandlerDeps{
			HostKey:            deps.SSHHostKey,
			Auth:               auth,
			AuthURL:            deps.AuthURL,
			AllowLocalForward:  deps.SSHAllowLocalForward,
			AllowRemoteForward: deps.SSHAllowRemoteForward,
			AllowAgentForward:  deps.SSHAllowAgentForward,
			SFTPEnabled:        deps.SFTPEnabled,
			Peers:              deps.PeerHosts,
			ForwardSockets:     deps.SSHForwardSockets,
			CloudboxBase:       deps.CloudboxBase,
			CloudboxProtocol:   deps.CloudboxProtocol,
			AccessToken:        deps.AccessToken,
			SelfName:           deps.SelfName,
		}))
	}

	// Reverse-proxy every method (GET/POST/WS upgrades included).
	rg.Any("/app/:name", apps.handler())
	rg.Any("/app/:name/*p", apps.handler())
}
