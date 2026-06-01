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
	"github.com/qiangli/outpost/internal/agent/upgrade"
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

	// upgrader + ledger gate the cloudbox-pushed upgrade tools
	// (outpost_rollback, outpost_upgrade_history). Nil on unpaired
	// hosts — the tools simply aren't registered there. Threaded in
	// by main.go alongside core.
	upgrader *upgrade.Worker
	ledger   *upgrade.Ledger

	// peersFn returns the daemon's current discovery cache snapshot.
	// Wired by main.go when discovery is on; nil otherwise (then the
	// outpost://peers resource just returns an empty list).
	peersFn func() any

	// gossipMembersFn returns the SWIM gossip member list. Wired by
	// main.go when gossip is up; nil otherwise.
	gossipMembersFn func() any
}

// Deps is what main.go threads in. Core is the shared admincore.Server
// instance also given to adminui. Token is the persisted bearer the
// caller must present; RotateFn is invoked when a tool requests a
// fresh value (returns the new token; mcpapi swaps s.token before
// responding).
//
// Upgrader + Ledger are the cloudbox-upgrade surface. Nil on unpaired
// hosts (and the corresponding tools simply don't register).
type Deps struct {
	Core     *admincore.Server
	Token    string
	Version  string
	RotateFn func() (string, error)
	Upgrader *upgrade.Worker
	Ledger   *upgrade.Ledger
	// PeersFn returns the daemon's current discovery cache snapshot
	// (typically discovery.Cache.Snapshot()). Nil when discovery is
	// off; the outpost://peers resource then returns an empty list.
	PeersFn func() any

	// GossipMembersFn returns the SWIM gossip member list (roadmap
	// item #17). Backs the outpost_gossip_edges MCP tool. Nil when
	// gossip is off.
	GossipMembersFn func() any
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
		core:            deps.Core,
		token:           deps.Token,
		rotateFn:        deps.RotateFn,
		upgrader:        deps.Upgrader,
		ledger:          deps.Ledger,
		peersFn:         deps.PeersFn,
		gossipMembersFn: deps.GossipMembersFn,
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

// Close terminates every open MCP session. Called on daemon shutdown
// before http.Server.Shutdown so the long-lived SSE streams the
// streamable transport keeps open don't block listener teardown
// until the 5-second timeout fires (forcing an SIGKILL fallback in
// `outpost stop`).
//
// Iterating Server.Sessions() during teardown is safe — no new
// sessions are created once the listener is shutting down, and the
// SDK guards the iterator against concurrent modification.
func (s *Server) Close() {
	if s.mcp == nil {
		return
	}
	for sess := range s.mcp.Sessions() {
		_ = sess.Close()
	}
}

// Rotate mints a fresh bearer (via rotateFn — typically
// conf.RotateMCPBearerToken), swaps s.token atomically, and returns the
// new value. The OLD token stops authenticating immediately. The
// admin UI's "Rotate" button and the outpost_rotate_mcp_token MCP tool
// both end up here so the in-memory state stays consistent regardless
// of which surface initiates the rotation.
func (s *Server) Rotate() (string, error) {
	if s.rotateFn == nil {
		return "", errMissingDep("RotateFn")
	}
	newTok, err := s.rotateFn()
	if err != nil {
		return "", err
	}
	s.token = newTok
	return newTok, nil
}

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
