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
	build, err := RevertToPrevious(st.BinaryPath, previous, w.ledger, LedgerEntry{
		Step:    "rollback",
		FromSHA: st.CurrentCommit,
		Detail:  "restored outpost.previous over live binary",
	})
	if err != nil {
		if errors.Is(err, ErrNoPrevious) {
			return RollbackResult{Status: "no_previous", Detail: ErrNoPrevious.Error()}, nil
		}
		return RollbackResult{}, err
	}

	// A manual rollback supersedes any pending auto-rollback watchdog —
	// the operator is in control now. (We deliberately do NOT quarantine
	// the rolled-back release here: the operator may want to re-apply it
	// after a fix, and the existing replay-seed already allows that.)
	_ = ClearPendingConfirm(w.confirmPath)

	w.restart()
	return RollbackResult{Previous: build, FromCommit: st.CurrentCommit}, nil
}

// RevertToPrevious swaps prevPath over binaryPath after probing prevPath, and
// (if a ledger is given) records the supplied entry with ToSHA filled from
// the restored build. It is the local, cloudbox-free core shared by the
// operator path (Worker.Rollback, step "rollback") and the auto-rollback
// watchdog (the supervisor, step "auto_rollback") — the supervisor can't call
// Worker.Rollback because the daemon is dead/crash-looping when it runs.
//
// It does NOT restart anything: Worker.Rollback re-execs the daemon
// afterwards, while the supervisor simply launches the now-reverted binary
// next. Returns ErrNoPrevious when prevPath is missing; refuses (returns an
// error without swapping) when the probe rejects a truncated / wrong-platform
// .previous, so a broken rollback target can't brick the host.
func RevertToPrevious(binaryPath, prevPath string, ledger *Ledger, entry LedgerEntry) (agent.BuildInfo, error) {
	if _, err := os.Stat(prevPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return agent.BuildInfo{}, ErrNoPrevious
		}
		return agent.BuildInfo{}, fmt.Errorf("stat previous: %w", err)
	}
	build, err := Probe(prevPath, "")
	if err != nil {
		return agent.BuildInfo{}, fmt.Errorf("verify rollback candidate: %w", err)
	}
	if err := os.Rename(prevPath, binaryPath); err != nil {
		return agent.BuildInfo{}, fmt.Errorf("swap rollback: %w", err)
	}
	if ledger != nil {
		entry.ToSHA = build.Short()
		_ = ledger.Append(entry)
	}
	return build, nil
}
