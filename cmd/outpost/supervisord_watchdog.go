package main

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/upgrade"
)

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
// Gated by FileConfig.AutoRollbackOn() (default OFF). When off, the watchdog
// still OBSERVES: it logs "WOULD auto-rollback …" so an operator can validate
// the signal on a canary before arming the destructive revert fleet-wide.
func watchdogPreStart(cacheDir string) func() error {
	return newWatchdogHook(
		upgrade.PendingConfirmPath(cacheDir),
		upgrade.NewLedger(filepath.Join(cacheDir, "upgrade.log")),
		upgrade.NewQuarantine(upgrade.QuarantinePath(cacheDir)),
		autoRollbackEnabled,
	)
}

// newWatchdogHook is the testable core: paths + an enabled predicate are
// injected so tests can drive it against a temp dir without the real config.
func newWatchdogHook(confirmPath string, ledger *upgrade.Ledger, quarantine *upgrade.Quarantine, enabled func() bool) func() error {
	return func() error {
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

		// Default-OFF: observe only. Keep the marker (so we keep logging the
		// signal each boot) and launch the current binary unchanged.
		if !enabled() {
			slog.Warn("watchdog: WOULD auto-rollback (observe mode; set auto_rollback_enabled=true to arm)",
				"release", pc.ReleaseID, "reason", reason, "prev", pc.PrevPath)
			_ = upgrade.WritePendingConfirm(confirmPath, *pc)
			return nil
		}

		// Armed: revert <binary>.previous over the live binary. Purely local —
		// no cloudbox round-trip (cloudbox may be down because of the very
		// storm this guards against).
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

// autoRollbackEnabled reads the live FileConfig for the destructive-revert
// opt-in. Read fresh each call so an operator flip takes effect on the next
// respawn without restarting the supervisor.
func autoRollbackEnabled() bool {
	cfgPath, err := conf.DefaultConfigPath()
	if err != nil {
		return false
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil || fc == nil {
		return false
	}
	return fc.AutoRollbackOn()
}
