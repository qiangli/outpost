package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/qiangli/outpost/internal/agent/upgrade"
)

// watchdogDeadline only observes the marker: polling must not count as a
// boot or write back a marker the daemon may have just cleared on confirmation.
// The supervisor stops the child first, then PreStart re-reads the marker and
// performs the same rollback as a crash/respawn, if still unconfirmed.
func watchdogDeadline(confirmPath string) func() bool {
	return func() bool {
		pc, err := upgrade.ReadPendingConfirm(confirmPath)
		if err != nil {
			slog.Warn("watchdog: read confirm marker", "err", err)
			return false
		}
		return pc != nil && time.Now().UTC().After(pc.ConfirmDeadline)
	}
}

// watchdogPreStart returns the supervisor PreStart hook that implements the
// auto-rollback watchdog's revert half. It runs IN THE SUPERVISOR PROCESS
// before each launch of the daemon — the only place that survives a daemon
// crash-loop and can revert a bad binary.
//
// Each call: if an upgrade left a pending-confirm marker (see
// internal/agent/upgrade/pending_confirm.go) that the new binary has NOT
// confirmed by staying up, count this respawn. Once the new binary has
// clearly failed to confirm (a crash-loop, MaxUnconfirmedBoots, OR the
// absolute confirm deadline passed), revert <binary>.previous over the live
// binary and quarantine the release so the puller doesn't re-pull it.
//
// ALWAYS ON: there is no opt-out. A just-upgraded build that does not
// confirm healthy is reverted, period — the 2026-10-24 downgrade incident
// (a downgrade swapped in with no rollback armed) must never strand a host again.
func watchdogPreStart(cacheDir string) func() error {
	return newWatchdogHook(
		upgrade.PendingConfirmPath(cacheDir),
		upgrade.NewLedger(filepath.Join(cacheDir, "upgrade.log")),
		upgrade.NewQuarantine(upgrade.QuarantinePath(cacheDir)),
	)
}

// newWatchdogHook is the testable core: paths are injected so tests can
// drive it against a temp dir without the real config.
func newWatchdogHook(confirmPath string, ledger *upgrade.Ledger, quarantine *upgrade.Quarantine) func() error {
	return func() error {
		if exe, err := os.Executable(); err == nil {
			if real, err := filepath.EvalSymlinks(exe); err == nil {
				exe = real
			}
			if recovered, err := upgrade.RecoverInterruptedPair(exe); err != nil {
				return fmt.Errorf("recover interrupted pair: %w", err)
			} else if recovered {
				if err := upgrade.ClearPendingConfirm(confirmPath); err != nil {
					return err
				}
			}
		}
		pc, err := upgrade.ReadPendingConfirm(confirmPath)
		if err != nil {
			return fmt.Errorf("read confirm marker: %w", err)
		}
		if pc == nil {
			return nil // no pending upgrade → normal launch
		}

		// This respawn is one more boot without a confirmation.
		pc.BootCount++

		crashLoop := pc.BootCount >= upgrade.MaxUnconfirmedBoots
		pastDeadline := time.Now().UTC().After(pc.ConfirmDeadline)
		if !crashLoop && !pastDeadline {
			// Still within the confirmation window — persist the bumped
			// boot_count and let the daemon try again to confirm.
			_ = upgrade.WritePendingConfirm(confirmPath, *pc)
			return nil
		}

		reason := fmt.Sprintf("new binary %s never confirmed healthy (boot_count=%d, deadline_passed=%t)",
			pc.ToSHA, pc.BootCount, pastDeadline)

		// Always on: revert <binary>.previous over the live binary.
		// Purely local — no cloudbox round-trip (cloudbox may be down
		// because of the very storm this guards against).
		if _, err := upgrade.RevertToPrevious(pc.BinaryPath, pc.PrevPath, ledger, upgrade.LedgerEntry{
			Step:      "auto_rollback",
			ReleaseID: pc.ReleaseID,
			FromSHA:   pc.ToSHA,
			Detail:    reason,
		}); err != nil {
			// .previous missing or itself broken (double-brick): do NOT swap
			// in a binary that won't run. Leave the marker, log loudly, and
			// launch the current binary — Phase-1 jitter keeps even a
			// permanent crash-loop a slow trickle, buying time for a fix.
			slog.Error("watchdog: auto-rollback FAILED; keeping current binary",
				"release", pc.ReleaseID, "err", err)
			_ = ledger.Append(upgrade.LedgerEntry{
				Step:      "auto_rollback_failed",
				ReleaseID: pc.ReleaseID,
				FromSHA:   pc.ToSHA,
				Error:     err.Error(),
			})
			_ = upgrade.WritePendingConfirm(confirmPath, *pc)
			return nil
		}

		// Reverted: quarantine the bad release (stops the puller re-pulling
		// it) and clear the marker. The supervisor now launches the
		// reverted (previous) binary.
		_ = quarantine.Add(upgrade.QuarantineEntry{
			ReleaseID: pc.ReleaseID,
			Commit:    pc.ToSHA,
			Reason:    reason,
		})
		_ = upgrade.ClearPendingConfirm(confirmPath)
		slog.Warn("watchdog: auto-rolled-back to previous binary",
			"release", pc.ReleaseID, "reason", reason)
		return nil
	}
}
