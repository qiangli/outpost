package upgrade

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/qiangli/outpost/internal/agent"
)

// RollbackResult is what Rollback returns to its caller (the MCP
// tool and CLI alike). Empty Status + Detail when the rollback was
// applied; non-empty Status when refused.
type RollbackResult struct {
	Status     string          `json:"status"` // "" on success, "no_previous"/"in_flight" on refusal
	Detail     string          `json:"detail,omitempty"`
	Previous   agent.BuildInfo `json:"previous,omitempty"` // build the rollback restored to
	FromCommit string          `json:"from_commit,omitempty"`
}

// ErrNoPrevious is returned when no rollback candidate is on disk
// (a fresh install or a daemon that hasn't been upgraded yet).
var ErrNoPrevious = errors.New("no outpost.previous on disk; nothing to roll back to")

// Rollback restores `<binary>.previous` over the live binary and
// triggers a restart. Refuses while another upgrade is in flight to
// avoid racing the inflight worker's own rename. After rollback the
// `.previous` file is gone; the operator must re-upgrade to climb
// forward again (we intentionally don't keep a "next" copy — that
// would require two-deep generation tracking we don't need today).
func (w *Worker) Rollback(ctx context.Context) (RollbackResult, error) {
	w.mu.Lock()
	if w.inFlight {
		w.mu.Unlock()
		return RollbackResult{Status: "in_flight", Detail: "an upgrade is currently running"}, nil
	}
	w.inFlight = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.inFlight = false
		w.mu.Unlock()
	}()

	st := w.state()
	if st.BinaryPath == "" {
		return RollbackResult{Status: "invalid", Detail: "daemon reports empty binary_path"}, nil
	}
	previous := st.BinaryPath + ".previous"
	if _, err := os.Stat(previous); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RollbackResult{Status: "no_previous", Detail: ErrNoPrevious.Error()}, nil
		}
		return RollbackResult{}, fmt.Errorf("stat previous: %w", err)
	}

	// Probe the previous binary before we trust it. A truncated or
	// platform-mismatched file would refuse to exec; we'd rather
	// abort than swap a broken binary into place and brick the host.
	build, err := Probe(previous, "")
	if err != nil {
		return RollbackResult{}, fmt.Errorf("verify rollback candidate: %w", err)
	}

	if err := os.Rename(previous, st.BinaryPath); err != nil {
		return RollbackResult{}, fmt.Errorf("swap rollback: %w", err)
	}

	_ = w.appendLedger(LedgerEntry{
		Step:    "rollback",
		FromSHA: st.CurrentCommit,
		ToSHA:   build.Short(),
		Detail:  "restored outpost.previous over live binary",
	})

	w.restart()
	return RollbackResult{Previous: build, FromCommit: st.CurrentCommit}, nil
}
