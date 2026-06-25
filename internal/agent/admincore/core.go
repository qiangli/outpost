// Package admincore holds the protocol-agnostic configuration operations
// outpost exposes — pairing, app CRUD, outbound mounts, built-in toggles,
// cluster kubeconfig, restart. Both the human-facing admin UI (HTTP +
// session cookie) and the agent-facing MCP server (HTTP + bearer token)
// dispatch into the same Server methods here, so validation rules and
// persistence semantics ship once.
//
// What lives here vs. in the HTTP layer:
//
//   - admincore: validate input, mutate FileConfig under a shared mutex,
//     update the live AppRegistry / OutboundManager, debounce restart.
//   - HTTP layer (adminui, mcpapi): authenticate the caller, parse the
//     wire format, translate admincore errors into the protocol's status
//     codes, render the response.
//
// Errors returned by admincore are *APIError when callers need to map
// them to a transport-level status, plain errors when the operation was
// unable to even start. HTTP wrappers use RespondError to translate.
package admincore

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/clusterllm"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/upgrade"
)

// Deps is what main.go threads into admincore.New. Everything here is
// concurrent-safe (or stateless): the Server doesn't own these values,
// it borrows them. AppRegistry and OutboundManager are live mutated
// across goroutines as the SPA / agent flips switches.
type Deps struct {
	// ConfigPath is where the persistent FileConfig lives. The Server
	// serializes all read-modify-write sequences against ConfigPath
	// under its own mutex.
	ConfigPath string

	// Apps is the live registry — admincore mutates it directly when
	// the operator adds/removes/toggles custom apps. Concurrent-safe.
	Apps *agent.AppRegistry

	// Outbound manages local mount paths that proxy through cloudbox
	// to remote outposts' apps. Optional — when nil the outbound
	// operations report "not configured" rather than panic.
	Outbound *agent.OutboundManager

	// Restart, when set, is invoked (debounced) after a save that
	// requires the tunnel or built-in routes to reload. Nil during
	// tests; admincore short-circuits ScheduleRestart in that case.
	Restart func()

	// CloudboxBase + CloudboxAccessToken + AgentName feed the outbound-
	// suggestions endpoint and the provisioning relay. CloudboxBase is
	// empty until pairing completes; admincore returns a clear error
	// instead of dialing nothing when the bearer is absent.
	CloudboxBase        string
	CloudboxAccessToken string
	AgentName           string

	// LLMPoolStatus, when set, returns the live pool diagnostic block
	// rendered into SafeView. Nil when the pool service wasn't wired
	// (Ollama off or daemon undetected). Closure rather than a concrete
	// type so admincore doesn't import the ollama package.
	LLMPoolStatus func() LLMPoolStatusView

	// PeerTiers, when set, returns the latest measured peer-locality tiers
	// (the p2p peer-plane probe's ground truth — TP/LAN/WAN per peer).
	// Closure so admincore doesn't import the peerplane package. Nil when
	// the service isn't wired.
	PeerTiers func() []PeerTierView

	// Upgrader + UpgradeLedger feed the Update tab on the admin UI
	// and the corresponding MCP tools. Nil on unpaired hosts (the
	// route falls back to a graceful 404 — see handlers/server.go
	// for the gate). Threaded through admincore so the surface
	// stays uniform across MCP / REST / future CLI.
	Upgrader      *upgrade.Worker
	UpgradeLedger *upgrade.Ledger

	// Backup, when set, is the live scheduler+worker for the folder-
	// watcher backup feature (admincore/backup.go). Optional — when
	// nil, SetBackup still persists the config to FileConfig (so a
	// future restart with the manager wired picks it up) but cannot
	// re-register the scheduler entry live.
	Backup BackupApplier
}

// LLMPoolStatusView is the wire shape rendered into SafeView. Kept here
// (rather than in the ollama package) so the HTTP layers can read it
// without taking on an ollama dependency.
type LLMPoolStatusView struct {
	Enabled     bool      `json:"enabled"`
	Running     bool      `json:"running"`
	LastPushAt  time.Time `json:"last_push_at,omitzero"`
	LastModels  int       `json:"last_models"`
	PushCount   int64     `json:"push_count"`
	LastError   string    `json:"last_error,omitempty"`
	MaxParallel int       `json:"max_parallel"`
	InFlight    int       `json:"in_flight"`
	CloudboxURL string    `json:"cloudbox_url,omitempty"`
	OllamaURL   string    `json:"ollama_url,omitempty"`
}

// PeerTierView is one peer's measured locality (rendered into SafeView + the
// outpost_peer_tiers MCP tool). Tier is GROUND TRUTH (measured RTT: "tp" <=2ms
// wired/dedicated, "lan" pipeline, "wan"/"unreached"); EgressSameLANHint is
// cloudbox's egress-IP guess, surfaced so operators see where the heuristic
// disagrees with the measurement.
type PeerTierView struct {
	Host              string    `json:"host"`
	Tier              string    `json:"tier"`
	RTTms             float64   `json:"rtt_ms"`
	Addr              string    `json:"addr,omitempty"`
	EgressSameLANHint bool      `json:"egress_same_lan_hint"`
	At                time.Time `json:"at,omitzero"`
}

// PeerTiers returns the latest measured peer-locality tiers, or nil when the
// peer-plane service isn't wired.
func (s *Server) PeerTiers() []PeerTierView {
	if s.deps.PeerTiers == nil {
		return nil
	}
	return s.deps.PeerTiers()
}

// Server is the stateful object that the HTTP layers share. Holds the
// FileConfig serialization mutex and the restart-debounce timer so that
// adminui and mcpapi calling the same operations in quick succession
// (e.g. the SPA toggling several builtins) collapse into a single save
// dance and a single restart.
type Server struct {
	deps Deps

	// mu serializes load-modify-save sequences against ConfigPath so
	// two concurrent UpsertApp calls don't race.
	mu sync.Mutex

	// restartMu + restartTimer debounce ScheduleRestart calls.
	restartMu    sync.Mutex
	restartTimer *time.Timer

	// detector caches podman/ollama availability probes so repeated
	// SafeView / Status reads don't hammer the local sockets.
	detector *agent.BuiltinDetector

	// clusterMu guards the lazily-built intra-home cluster-backend
	// detector. Rebuilt when the configured endpoint/key changes so
	// SafeView reflects current config (the endpoint change restarts the
	// daemon, but a SafeView read between save and restart still wants the
	// fresh detector). clusterDet caches probes for its own TTL.
	clusterMu  sync.Mutex
	clusterDet *clusterllm.Detector
	clusterKey string
}

// New constructs an admincore.Server. Deps.ConfigPath is required; other
// fields are optional (nil-checked at the call sites that need them).
func New(deps Deps) (*Server, error) {
	if deps.ConfigPath == "" {
		return nil, errors.New("admincore: ConfigPath required")
	}
	if deps.Apps == nil {
		deps.Apps = agent.NewAppRegistry()
	}
	return &Server{
		deps:     deps,
		detector: agent.NewBuiltinDetector(5 * time.Second),
	}, nil
}

// Deps returns the underlying dependency struct (read-only access for
// HTTP layers that need e.g. AgentName or CloudboxBase).
func (s *Server) Deps() Deps { return s.deps }

// SetCloudbox updates the cloudbox base URL + access token + agent name
// after a re-pair (Pair mutates the FileConfig but the in-memory deps
// snapshot is stale until callers refresh it). HTTP layers call this
// after Pair returns successfully.
func (s *Server) SetCloudbox(base, accessToken, agentName string) {
	s.deps.CloudboxBase = base
	s.deps.CloudboxAccessToken = accessToken
	s.deps.AgentName = agentName
}

// ScheduleRestart asynchronously triggers Deps.Restart after a short
// debounce so the in-flight HTTP response has time to flush AND so
// multiple back-to-back operations (the SPA auto-saves on every
// toggle) collapse into a single re-exec. Each call resets the timer.
func (s *Server) ScheduleRestart() {
	if s.deps.Restart == nil {
		return
	}
	s.restartMu.Lock()
	defer s.restartMu.Unlock()
	if s.restartTimer != nil {
		s.restartTimer.Stop()
	}
	s.restartTimer = time.AfterFunc(time.Second, s.deps.Restart)
}

// loadConfig reads the FileConfig (or returns an empty one on first
// run). Callers must hold s.mu when intending to write back.
func (s *Server) loadConfig() (*conf.FileConfig, error) {
	fc, err := conf.LoadFile(s.deps.ConfigPath)
	if err != nil {
		return nil, err
	}
	if fc == nil {
		fc = &conf.FileConfig{}
	}
	return fc, nil
}

// LoadConfig is the exported read-only variant. HTTP layers use it for
// pure renders (GET /api/config, MCP resource reads) that don't need to
// hold the save mutex. Returns a copy view; mutators must go through
// the typed operations.
func (s *Server) LoadConfig() (*conf.FileConfig, error) {
	return s.loadConfig()
}

// APIError carries an HTTP-style status alongside the human message so
// adminui can map it back to a gin status code and mcpapi can render an
// MCP-conformant error response.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

// HTTPStatus returns the suggested HTTP status code for this error.
func (e *APIError) HTTPStatus() int {
	if e == nil || e.Status == 0 {
		return http.StatusInternalServerError
	}
	return e.Status
}

func badRequest(format string, args ...any) error {
	return &APIError{Status: http.StatusBadRequest, Msg: fmt.Sprintf(format, args...)}
}
func notFound(format string, args ...any) error {
	return &APIError{Status: http.StatusNotFound, Msg: fmt.Sprintf(format, args...)}
}
func conflict(format string, args ...any) error {
	return &APIError{Status: http.StatusConflict, Msg: fmt.Sprintf(format, args...)}
}
func unavailable(format string, args ...any) error {
	return &APIError{Status: http.StatusServiceUnavailable, Msg: fmt.Sprintf(format, args...)}
}
func upstream(format string, args ...any) error {
	return &APIError{Status: http.StatusBadGateway, Msg: fmt.Sprintf(format, args...)}
}
func internalErr(format string, args ...any) error {
	return &APIError{Status: http.StatusInternalServerError, Msg: fmt.Sprintf(format, args...)}
}

// AsAPIError unwraps err into an *APIError if it is one (directly or
// via errors.As). Returns nil otherwise. HTTP layers call this to pick
// the right status code; plain (non-APIError) errors should be treated
// as 500.
func AsAPIError(err error) *APIError {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae
	}
	return nil
}
