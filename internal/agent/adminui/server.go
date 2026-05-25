// Package adminui serves the local-only configuration web UI for outpost.
//
// The server binds its own loopback listener (default 127.0.0.1:17777,
// override via $OUTPOST_ADMIN_ADDR). It is intentionally not part of the
// main HTTP server that the matrix tunnel proxies — admin is local-only by
// design.
//
// On first run (no config file on disk yet) the admin API is unauthenticated:
// the listener is loopback-only, so any reachable caller is the OS user who
// just launched the binary. Once a config exists the gate engages and every
// API call must carry an outpost_admin session cookie minted by POST
// /api/login (which verifies the running OS user's password via hostauth).
//
// Business-logic operations (validate / mutate FileConfig / mutate live
// registries / debounce restart) live in internal/agent/admincore so the
// MCP server can share them. This package is now a thin HTTP layer:
// session-cookie auth, JSON binding, error→status mapping.
package adminui

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/hostauth"
)

// DefaultAdminAddr is the loopback listener address the admin UI binds
// when neither $OUTPOST_ADMIN_ADDR nor --admin-addr is set.
const DefaultAdminAddr = "127.0.0.1:17777"

// LLMPoolStatusView is re-exported from admincore so existing callers
// (main.go, tests) keep working without an import shuffle.
type LLMPoolStatusView = admincore.LLMPoolStatusView

// Suggestion is re-exported from admincore for backward compat with
// tests that reference the name unqualified.
type Suggestion = admincore.Suggestion

// builtinView / clusterView aliases keep test reflection on these field
// types working after the move into admincore.
type builtinView = admincore.BuiltinView
type clusterView = admincore.ClusterView

// outboundUpsertReq + validateOutbound aliases keep the legacy
// outbound_validate_test.go calling shape green.
type outboundUpsertReq = admincore.OutboundParams

var validateOutbound = admincore.ValidateOutbound

// Deps is what main.go threads in when constructing the admin server.
//
// Two construction modes:
//
//  1. Production (main.go): supply Core, plus the HTTP-only fields
//     (ListenAddr, Auth, SessionKey). The admincore.Server is shared
//     with mcpapi so both surfaces see the same live state, the same
//     file-save mutex, and the same restart-debounce timer.
//
//  2. Test convenience: leave Core nil and supply the legacy
//     fields (ConfigPath, Apps, Outbound, ...). New() then
//     constructs an admincore.Server internally.
type Deps struct {
	// Core, when non-nil, is the shared admincore.Server that mutation
	// handlers dispatch into. When nil, New() builds one from the
	// legacy fields below — the test path.
	Core *admincore.Server

	// HTTP-layer concerns (always read directly by adminui):

	// ListenAddr is the loopback address+port the admin server binds.
	// Defaults to 127.0.0.1:17777 if empty.
	ListenAddr string
	// Auth verifies the running OS user's password on POST /api/login.
	Auth hostauth.Authenticator
	// SessionKey is the HMAC secret used to sign admin-UI session
	// cookies. Persisting it across restarts is what keeps the admin
	// logged in when a built-in toggle re-execs the binary.
	SessionKey []byte

	// Legacy fields — forwarded into admincore.New() when Core is nil.
	// Production main.go leaves these empty and supplies Core.

	ConfigPath          string
	Apps                *agent.AppRegistry
	Outbound            *agent.OutboundManager
	Restart             func()
	CloudboxBase        string
	CloudboxAccessToken string
	AgentName           string
	LLMPoolStatus       func() LLMPoolStatusView
}

// Server is the admin HTTP server. Construct with New, then call Serve.
type Server struct {
	deps   Deps
	core   *admincore.Server
	engine *gin.Engine

	listener net.Listener
	sessions *sessionStore
	srv      *http.Server

	// loopbackOnly is true when the bound listener address resolved to a
	// loopback IP (127.0.0.0/8 or ::1). When false, the admin UI is reachable
	// from the LAN, which disables the first-run "no config yet → no auth"
	// bypass in requireSession.
	loopbackOnly bool

	// loginRL throttles POST /api/login by client IP — defense in depth
	// against LAN brute-force attempts now that the listener can bind
	// non-loopback. Pre-existing PAM rate-limiting is also in play.
	loginRL *loginLimiter

	// muLocalProxy serializes NoRoute lookups; kept for backwards-compat
	// with previous structure (not strictly required since the registries
	// are concurrent-safe on their own).
	muLocalProxy sync.Mutex
}

// isLoopbackAddr reports whether addr (the resolved bound address) is on
// a loopback interface.
func isLoopbackAddr(addr net.Addr) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// New builds the admin server and binds its listener. The returned Server
// is not yet serving — call Serve.
func New(deps Deps) (*Server, error) {
	if deps.Auth == nil {
		deps.Auth = hostauth.DefaultAuthenticator()
	}
	if deps.Apps == nil && deps.Core == nil {
		deps.Apps = agent.NewAppRegistry()
	}
	if deps.ListenAddr == "" {
		deps.ListenAddr = DefaultAdminAddr
	}

	// Production callers thread a shared admincore.Server through Deps.Core
	// so adminui and mcpapi share live state. Test callers leave Core nil;
	// we build one here from the legacy fields.
	if deps.Core == nil {
		if deps.ConfigPath == "" {
			return nil, errors.New("adminui: ConfigPath required (or supply Deps.Core)")
		}
		core, err := admincore.New(admincore.Deps{
			ConfigPath:          deps.ConfigPath,
			Apps:                deps.Apps,
			Outbound:            deps.Outbound,
			Restart:             deps.Restart,
			CloudboxBase:        deps.CloudboxBase,
			CloudboxAccessToken: deps.CloudboxAccessToken,
			AgentName:           deps.AgentName,
			LLMPoolStatus:       deps.LLMPoolStatus,
		})
		if err != nil {
			return nil, err
		}
		deps.Core = core
	}
	// Mirror the core's deps into adminui.Deps so backward-compat
	// accessors (s.deps.Apps, s.deps.ConfigPath) used by the middleware
	// and by existing tests keep resolving without ferrying every
	// access through s.core.Deps().
	cd := deps.Core.Deps()
	if deps.ConfigPath == "" {
		deps.ConfigPath = cd.ConfigPath
	}
	if deps.Apps == nil {
		deps.Apps = cd.Apps
	}
	if deps.Outbound == nil {
		deps.Outbound = cd.Outbound
	}
	if deps.AgentName == "" {
		deps.AgentName = cd.AgentName
	}
	if deps.CloudboxBase == "" {
		deps.CloudboxBase = cd.CloudboxBase
	}
	if deps.CloudboxAccessToken == "" {
		deps.CloudboxAccessToken = cd.CloudboxAccessToken
	}
	if deps.LLMPoolStatus == nil {
		deps.LLMPoolStatus = cd.LLMPoolStatus
	}

	gin.SetMode(gin.ReleaseMode)
	eng := gin.New()
	eng.Use(gin.Recovery())

	s := &Server{
		deps:     deps,
		core:     deps.Core,
		engine:   eng,
		sessions: newSessionStore(time.Hour, deps.SessionKey),
		loginRL:  newLoginLimiter(5, 12*time.Second),
	}
	s.registerRoutes()

	ln, err := net.Listen("tcp", deps.ListenAddr)
	if err != nil {
		return nil, err
	}
	s.listener = ln
	s.loopbackOnly = isLoopbackAddr(ln.Addr())
	if !s.loopbackOnly {
		slog.Warn("outpost admin UI is reachable from the network — login is always required",
			"bind", ln.Addr().String())
	}
	s.srv = &http.Server{
		Handler:           eng,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

// Addr returns the bound loopback address.
func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Engine returns the underlying gin engine. mcpapi mounts its handler
// at /mcp/* here so MCP and admin UI share one loopback listener (one
// URL the operator copy-pastes, one port the firewall sees). The
// bearer-token middleware lives inside mcpapi — adminui doesn't see
// /mcp/* traffic; the route is registered after adminui's own routes,
// so admin's session-cookie middleware never inspects it.
func (s *Server) Engine() *gin.Engine { return s.engine }

// URL is a convenience for the message printed to the console.
func (s *Server) URL() string {
	return "http://" + s.Addr()
}

// Serve runs until ctx is canceled, then shuts the HTTP server down.
func (s *Server) Serve(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		err := s.srv.Serve(s.listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
		return <-errCh
	case err := <-errCh:
		return err
	}
}

// outboundEnabled reports whether the OutboundManager was wired (only
// then do we register the outbound sub-routes).
func (s *Server) outboundEnabled() bool {
	return s.core.Deps().Outbound != nil
}

func (s *Server) registerRoutes() {
	s.engine.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	// Static SPA + assets.
	s.mountUI()

	// Provisioning relay — outside the session cookie gate (per-app
	// bearer auth lives in agent.RegisterProvisionRoutes).
	cd := s.core.Deps()
	agent.RegisterProvisionRoutes(s.engine, agent.ProvisionDeps{
		Apps:         cd.Apps,
		CloudboxBase: cd.CloudboxBase,
		AccessToken:  cd.CloudboxAccessToken,
		AgentName:    cd.AgentName,
	})

	// Login lives outside the gate.
	s.engine.POST("/api/login", s.handleLogin)

	api := s.engine.Group("/api", s.requireSession())
	api.GET("/status", s.handleStatus)
	api.POST("/logout", s.handleLogout)
	api.GET("/config", s.handleGetConfig)
	api.POST("/config/register", s.handleRegister)
	api.POST("/config/builtins", s.handleBuiltins)
	api.GET("/apps", s.handleListApps)
	api.GET("/apps/suggestions", s.handleListSuggestions)
	api.POST("/apps", s.handleUpsertApp)
	api.DELETE("/apps/:name", s.handleDeleteApp)
	api.POST("/apps/:name/provisioning-token/rotate", s.handleRotateProvisioningToken)
	api.POST("/restart", s.handleRestart)
	api.POST("/cluster/kubeconfig", s.handleSetClusterKubeconfig)
	api.DELETE("/cluster/kubeconfig", s.handleClearClusterKubeconfig)

	if s.outboundEnabled() {
		api.GET("/outbound", s.handleListOutbound)
		api.GET("/outbound/suggestions", s.handleOutboundSuggestions)
		api.POST("/outbound", s.handleAddOutbound)
		api.DELETE("/outbound/:path", s.handleDeleteOutbound)
		api.POST("/outbound/:path/connect", s.handleConnectOutbound)
		api.POST("/outbound/:path/disconnect", s.handleDisconnectOutbound)
	}

	// Local-access proxy via NoRoute fallback (session-gated).
	s.engine.NoRoute(s.handleLocalAppProxy)
}

// handleLocalAppProxy is the NoRoute fallback. Strips the first path
// segment as the app/outbound name and forwards to the corresponding
// registry. Requires a valid admin session cookie.
func (s *Server) handleLocalAppProxy(c *gin.Context) {
	p := strings.TrimPrefix(c.Request.URL.Path, "/")
	if p == "" {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	name, rest, hasRest := strings.Cut(p, "/")
	if name == "" {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	cd := s.core.Deps()
	outboundMatch := cd.Outbound != nil && cd.Outbound.Has(name)
	localMatch := cd.Apps != nil && cd.Apps.LookupTarget(name) != nil
	if !outboundMatch && !localMatch {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if cookie, err := c.Cookie(cookieName); err != nil || cookie == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "login required"})
		return
	} else if _, ok := s.sessions.Validate(cookie); !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "session expired"})
		return
	}
	upstreamPath := "/"
	if hasRest {
		upstreamPath = "/" + rest
	}
	if outboundMatch {
		cd.Outbound.ProxyTo(c, name, upstreamPath)
		return
	}
	cd.Apps.ProxyTo(c, name, upstreamPath)
}
