// Package mcpapi exposes outpost's configuration surface to agent tools
// (Claude Code, Windsurf, the outpost CLI, ...) over the Model Context
// Protocol. It is mounted as an http.Handler at /mcp/* on the same
// loopback listener the admin UI uses (default 127.0.0.1:17777), with
// a separate bearer-token auth gate so the two surfaces never share
// credentials.
//
// Every MCP tool registered here is a thin call into
// internal/agent/admincore — the same business-logic layer the admin
// UI's HTTP handlers dispatch into. Validation, FileConfig mutation,
// live AppRegistry / OutboundManager updates, and restart debouncing
// happen once, regardless of which surface the operator chose.
//
// Auth model: shared bearer token persisted in
// FileConfig.MCPBearerToken (mode 0600 same as the admin session key).
// The operator copies the token into their .mcp.json:
//
//	{
//	  "outpost": {
//	    "type": "http",
//	    "url": "http://127.0.0.1:17777/mcp/",
//	    "headers": {"Authorization": "Bearer <token>"}
//	  }
//	}
package mcpapi

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// Server wraps the modelcontextprotocol/go-sdk MCP server with outpost-
// specific tool registrations and a static bearer-token auth gate.
type Server struct {
	core    *admincore.Server
	token   string
	mcp     *mcp.Server
	handler http.Handler

	// rotateFn, when set, is called by the outpost.rotate_mcp_token tool
	// to mint a fresh token, persist it, and (most importantly) swap
	// s.token atomically so subsequent calls authenticate against the
	// new value. Wired in by main.go because the persistence path
	// (FileConfig.MCPBearerToken) lives outside mcpapi.
	rotateFn func() (string, error)
}

// Deps is what main.go threads in. Core is the shared admincore.Server
// instance also given to adminui. Token is the persisted bearer the
// caller must present; RotateFn is invoked when a tool requests a
// fresh value (returns the new token; mcpapi swaps s.token before
// responding).
type Deps struct {
	Core     *admincore.Server
	Token    string
	Version  string
	RotateFn func() (string, error)
}

// New constructs the MCP server and registers every parity tool. Call
// Handler() to obtain the http.Handler to mount under /mcp/* on the
// shared listener.
func New(deps Deps) (*Server, error) {
	if deps.Core == nil {
		return nil, errMissingDep("Core")
	}
	if deps.Token == "" {
		return nil, errMissingDep("Token")
	}
	version := deps.Version
	if version == "" {
		version = "dev"
	}
	s := &Server{
		core:     deps.Core,
		token:    deps.Token,
		rotateFn: deps.RotateFn,
	}
	s.mcp = mcp.NewServer(&mcp.Implementation{
		Name:    "outpost",
		Version: version,
		Title:   "Outpost configuration",
	}, nil)
	s.registerTools()
	s.registerResources()

	inner := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.mcp },
		nil,
	)
	s.handler = s.bearerAuth(inner)
	return s, nil
}

// Handler returns the http.Handler that the shared gin engine mounts
// at /mcp/*. The bearer-token middleware is already wrapped in.
func (s *Server) Handler() http.Handler { return s.handler }

// Token returns the bearer token currently accepted by the server.
// Surfaced for the admin UI's "show MCP credentials" panel and for the
// startup banner.
func (s *Server) Token() string { return s.token }

// bearerAuth is the middleware that gates every request to the MCP
// handler. Constant-time comparison so a bad token can't be probed via
// timing.
//
// Token rotation flow: the outpost.rotate_mcp_token tool calls
// s.rotateFn (which writes the new token to FileConfig and returns
// it), then s.token is swapped atomically so the next request uses
// the new value. Until the operator updates their .mcp.json, the old
// token returns 401.
func (s *Server) bearerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimSpace(r.Header.Get("Authorization"))
		const prefix = "Bearer "
		if !strings.HasPrefix(got, prefix) {
			http.Error(w, `{"error":"bearer token required"}`, http.StatusUnauthorized)
			return
		}
		got = strings.TrimSpace(got[len(prefix):])
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, `{"error":"invalid bearer token"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func errMissingDep(name string) error {
	return &missingDepError{name: name}
}

type missingDepError struct{ name string }

func (e *missingDepError) Error() string { return "mcpapi: missing required dep " + e.name }
