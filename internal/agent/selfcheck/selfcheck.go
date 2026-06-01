// Package selfcheck owns the Layer-2 defense: detect partial outpost
// corruption and self-heal from durable inputs.
//
// Runs at boot + every 5 min. Each pass writes one structured event
// per failed invariant; the cloudbox-side heartbeat path (Layer 5)
// reads Status() to surface "selfcheck=fail" up to the operator
// dashboard.
//
// Scope kept narrow — anything we can fix locally + cheaply gets
// fixed; anything that needs operator decision (e.g. corrupted host
// key, which would invalidate every peer's known_hosts) is reported
// loudly but NOT auto-rotated.
package selfcheck

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// Status enum reported to the heartbeat path.
const (
	StatusOK   = "ok"
	StatusWarn = "warn"
	StatusFail = "fail"
)

// CheckResult is one pass's outcome.
type CheckResult struct {
	Status  string    `json:"status"`
	At      time.Time `json:"at"`
	Issues  []string  `json:"issues,omitempty"`
	Repairs []string  `json:"repairs,omitempty"`
}

// Server is the long-running worker. One per outpost.
type Server struct {
	cfgPath string
	mu      sync.Mutex
	last    atomic.Pointer[CheckResult]
}

// New constructs the worker. cfgPath is the on-disk agent.json path
// (DefaultConfigPath()). The Worker itself is cheap; Run does the work.
func New(cfgPath string) *Server {
	s := &Server{cfgPath: cfgPath}
	s.last.Store(&CheckResult{Status: StatusOK, At: time.Now().UTC()})
	return s
}

// LastStatus returns the most recent check outcome (StatusOK/Warn/Fail).
// Safe to call from any goroutine.
func (s *Server) LastStatus() string {
	cr := s.last.Load()
	if cr == nil {
		return StatusOK
	}
	return cr.Status
}

// LastResult returns the most recent full CheckResult.
func (s *Server) LastResult() CheckResult {
	cr := s.last.Load()
	if cr == nil {
		return CheckResult{Status: StatusOK, At: time.Now().UTC()}
	}
	return *cr
}

// Run drives the boot pass + 5min tick. Blocks until ctx.Done().
func (s *Server) Run(ctx context.Context) error {
	// Boot pass — slight jitter so we don't burn CPU at the exact
	// moment the daemon is binding its listeners.
	time.Sleep(3 * time.Second)
	s.runOnce()

	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.runOnce()
		}
	}
}

// runOnce executes one validation pass + auto-repair where safe.
func (s *Server) runOnce() {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := CheckResult{Status: StatusOK, At: time.Now().UTC()}

	// 1. agent.json must parse.
	fc, err := conf.LoadFile(s.cfgPath)
	if err != nil {
		// Try restoring from journal.
		idx, restErr := conf.RestoreLatestValid(s.cfgPath)
		if restErr != nil {
			r.Status = StatusFail
			r.Issues = append(r.Issues, "agent.json parse failure: "+err.Error()+
				"; journal restore failed: "+restErr.Error())
		} else if idx > 0 {
			r.Status = StatusWarn
			r.Repairs = append(r.Repairs, "restored agent.json from journal slot "+itoa(idx))
			// Reload after restore.
			fc, err = conf.LoadFile(s.cfgPath)
			if err != nil {
				r.Status = StatusFail
				r.Issues = append(r.Issues, "agent.json still unreadable after restore: "+err.Error())
			}
		} else {
			r.Status = StatusFail
			r.Issues = append(r.Issues, "agent.json parse failure with no journal snapshot to fall back on: "+err.Error())
		}
	}

	if fc != nil {
		// 2. SSH host key must exist (the in-process /ssh server
		//    needs it; absence would cause known_hosts churn on
		//    every peer). Don't auto-regenerate — that's a
		//    user-visible event (clients see REMOTE HOST IDENT
		//    CHANGED). Just report.
		if k := s.sshHostKeyPath(); k != "" {
			if _, err := os.Stat(k); err != nil {
				r.Status = StatusWarn
				r.Issues = append(r.Issues,
					"ssh host key missing at "+k+
						"; in-process /ssh will fail until regenerated (run `outpost restart`)")
			}
		}

		// 3. MCP bearer token must be non-empty (the admin/agent
		//    listener is unauthenticated without it on LAN binds).
		//    Safe to auto-regenerate — operator just needs to
		//    re-copy from the SPA / `outpost mcp endpoint`.
		if fc.MCPBearerToken == "" {
			if _, err := conf.EnsureMCPBearerToken(s.cfgPath, fc); err == nil {
				r.Repairs = append(r.Repairs, "regenerated empty MCP bearer token")
				if r.Status == StatusOK {
					r.Status = StatusWarn
				}
			} else {
				r.Status = StatusFail
				r.Issues = append(r.Issues, "MCP bearer regen failed: "+err.Error())
			}
		}

		// 4. Admin session key must be non-empty (the admin UI
		//    can't sign cookies otherwise; new admin logins would
		//    fail with "invalid signature"). Safe to regenerate
		//    — every active SPA session is killed, but the
		//    operator just re-logs in.
		if len(fc.AdminSessionKey) == 0 {
			if _, err := conf.EnsureAdminSessionKey(s.cfgPath, fc); err == nil {
				r.Repairs = append(r.Repairs, "regenerated empty admin session key (active SPA sessions invalidated)")
				if r.Status == StatusOK {
					r.Status = StatusWarn
				}
			} else {
				r.Status = StatusFail
				r.Issues = append(r.Issues, "admin session key regen failed: "+err.Error())
			}
		}
	}

	if r.Status != StatusOK {
		slog.Warn("selfcheck: pass not clean",
			"status", r.Status, "issues", r.Issues, "repairs", r.Repairs)
	}
	s.last.Store(&r)
}

// sshHostKeyPath mirrors internal/agent/hostkey.go's path resolution.
// Re-derived here (rather than imported) to avoid a circular import —
// agent → selfcheck → agent. The path is canon (since 2025-Q3) and
// kept in lockstep here + in hostkey.go.
func (s *Server) sshHostKeyPath() string {
	cfgDir := filepath.Dir(s.cfgPath)
	return filepath.Join(cfgDir, "ssh_host_ed25519")
}

func itoa(i int) string {
	switch i {
	case 0:
		return "0"
	case 1:
		return "1"
	case 2:
		return "2"
	case 3:
		return "3"
	}
	// Generic fallback — unlikely (ring is bounded).
	if i < 0 {
		return "-?"
	}
	return "?"
}

// Errors callers may want to test against.
var (
	ErrNoConfig = errors.New("selfcheck: agent.json missing")
)
