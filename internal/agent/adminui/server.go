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
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/hostauth"
)

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
}

// Server is the admin HTTP server. Construct with New, then call Serve.
type Server struct {
	deps     Deps
	engine   *gin.Engine
	listener net.Listener
	sessions *sessionStore
	srv      *http.Server

	// mu serializes load-modify-save sequences on the on-disk FileConfig
	// so two concurrent POSTs to /api/apps don't race.
	mu sync.Mutex
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
		deps.ListenAddr = "127.0.0.1:17777"
	}

	gin.SetMode(gin.ReleaseMode)
	eng := gin.New()
	eng.Use(gin.Recovery())

	s := &Server{
		deps:     deps,
		engine:   eng,
		sessions: newSessionStore(time.Hour),
	}
	s.registerRoutes()

	ln, err := net.Listen("tcp", deps.ListenAddr)
	if err != nil {
		return nil, err
	}
	s.listener = ln
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
}
