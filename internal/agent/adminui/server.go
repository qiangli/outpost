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
	"github.com/qiangli/outpost/internal/agent/hostauth"
)

// DefaultAdminAddr is the loopback listener address the admin UI binds
// when neither $OUTPOST_ADMIN_ADDR nor --admin-addr is set. The port is
// chosen to be unprivileged (> 1024 so no root needed), below every
// supported OS's ephemeral range (so it isn't transiently grabbed by an
// outbound connection before bind), IANA-unregistered (no collision with
// a documented service), and outside common dev-tool squats like 8080 /
// 8888 / 9999 — relevant once an operator binds the admin UI to a LAN
// address instead of loopback. Operators who need to move it should
// override via $OUTPOST_ADMIN_ADDR or --admin-addr rather than editing
// this constant, since the value is referenced by existing pairings,
// bookmarks, and CLAUDE.md.
const DefaultAdminAddr = "127.0.0.1:17777"

// Deps is what main.go threads in when constructing the admin server.
type Deps struct {
	// ConfigPath is where to read and write the persistent FileConfig
	// (typically conf.DefaultConfigPath()).
	ConfigPath string
	// ListenAddr is the loopback address+port the admin server binds.
	// Defaults to 127.0.0.1:17777 if empty.
	ListenAddr string
	// Auth verifies the running OS user's password on POST /api/login.
	Auth hostauth.Authenticator
	// Apps is the live registry — admin handlers add/remove entries here
	// without process restart for custom app changes.
	Apps *agent.AppRegistry
	// Restart, when invoked, exits this process and re-execs the binary.
	// Called after saves that require the tunnel or built-in routes to
	// reload (pairing/server URL/agent name/built-in toggles).
	Restart func()
	// SessionKey is the HMAC secret used to sign admin-UI session
	// cookies. Persisting it across restarts is what keeps the admin
	// logged in when a built-in toggle re-execs the binary. main.go
	// loads/generates it via conf.EnsureAdminSessionKey.
	SessionKey []byte
	// Outbound is the manager for `/<path>/...` mounts that proxy
	// through cloudbox to remote outposts' apps. Optional — when nil
	// the admin UI surface omits the Outbound section and the
	// local-proxy NoRoute handler skips the outbound lookup. main.go
	// constructs it once per boot and threads it in.
	Outbound *agent.OutboundManager
}

// Server is the admin HTTP server. Construct with New, then call Serve.
type Server struct {
	deps     Deps
	engine   *gin.Engine
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

	// detector caches podman/ollama availability probes so repeated
	// /api/config and /api/status calls don't hammer the local sockets.
	detector *agent.BuiltinDetector

	// mu serializes load-modify-save sequences on the on-disk FileConfig
	// so two concurrent POSTs to /api/apps don't race.
	mu sync.Mutex

	// restartMu + restartTimer debounce scheduleRestart calls: the admin
	// UI flips multiple builtin toggles in quick succession (now that each
	// switch auto-saves), and each one would otherwise re-exec the process.
	// Collapse them into a single restart by resetting the timer on every
	// new request.
	restartMu    sync.Mutex
	restartTimer *time.Timer
}

// isLoopbackAddr reports whether addr (the resolved bound address) is on
// a loopback interface. IPv6 unspecified (::) listens on both loopback
// and non-loopback, so we treat unspecified addresses as non-loopback —
// the conservative choice for the auth gate.
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
	if deps.ConfigPath == "" {
		return nil, errors.New("adminui: ConfigPath required")
	}
	if deps.Auth == nil {
		deps.Auth = hostauth.DefaultAuthenticator()
	}
	if deps.Apps == nil {
		deps.Apps = agent.NewAppRegistry()
	}
	if deps.ListenAddr == "" {
		deps.ListenAddr = DefaultAdminAddr
	}

	gin.SetMode(gin.ReleaseMode)
	eng := gin.New()
	eng.Use(gin.Recovery())

	s := &Server{
		deps:     deps,
		engine:   eng,
		sessions: newSessionStore(time.Hour, deps.SessionKey),
		loginRL:  newLoginLimiter(5, 12*time.Second),
		detector: agent.NewBuiltinDetector(5 * time.Second),
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

// Addr returns the bound loopback address (after net.Listen resolved any
// :0 port).
func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

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

func (s *Server) registerRoutes() {
	s.engine.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	// Static SPA + assets (always served — the SPA itself decides what to
	// render based on /api/status).
	s.mountUI()

	// Login lives outside the gate (callers wouldn't have a session yet).
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
	api.POST("/restart", s.handleRestart)

	// Outbound — local mounts that proxy through cloudbox to remote
	// outposts' apps. Only registered when an OutboundManager was
	// supplied via Deps.
	if s.deps.Outbound != nil {
		api.GET("/outbound", s.handleListOutbound)
		api.GET("/outbound/suggestions", s.handleOutboundSuggestions)
		api.POST("/outbound", s.handleAddOutbound)
		api.DELETE("/outbound/:path", s.handleDeleteOutbound)
		api.POST("/outbound/:path/connect", s.handleConnectOutbound)
		api.POST("/outbound/:path/disconnect", s.handleDisconnectOutbound)
	}

	// Local-access proxy: serve each registered app at the admin UI's
	// own listener as `/<name>/...`, gated by the same session cookie.
	// Lets users reach e.g. Ollama at http://localhost:17777/ollama/
	// without going through the cloudbox tunnel. Outbound mounts have
	// precedence over local apps when their names collide.
	//
	// Implemented via NoRoute so we don't fight with the static API/SPA
	// routes above for the same prefix tree.
	s.engine.NoRoute(s.handleLocalAppProxy)
}

// handleLocalAppProxy is the NoRoute fallback. It strips the first path
// segment as the app name and forwards the rest to the AppRegistry's
// proxy. Requires a valid admin session cookie — same gate as everything
// else on this listener. Unknown app names 404.
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
	// Resolve precedence: outbound mounts win over local apps if the name
	// matches both. (handleAddOutbound rejects shadowing a local-app
	// name, so this only matters when the operator manually edits the
	// config file.)
	outboundMatch := s.deps.Outbound != nil && s.deps.Outbound.Has(name)
	localMatch := s.deps.Apps != nil && s.deps.Apps.LookupTarget(name) != nil
	if !outboundMatch && !localMatch {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	// Auth: session cookie required. We can't use the requireSession
	// middleware here because NoRoute's chain is one-shot.
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
		s.deps.Outbound.ProxyTo(c, name, upstreamPath)
		return
	}
	s.deps.Apps.ProxyTo(c, name, upstreamPath)
}
