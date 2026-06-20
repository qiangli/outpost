package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Auto-rollback watchdog ("confirm-healthy-or-revert"), the A/B-update
// pattern (ChromeOS update_engine mark-boot-successful / systemd-bless-boot):
// after a self-upgrade swaps the binary, the NEW binary must prove itself
// healthy within a deadline or the supervisor reverts to <binary>.previous.
//
// The health proof is deliberately LOCAL and simple: the new binary stayed
// up for confirmDwell without the process dying. That is exactly what a
// brick (panic-on-boot, bad cross-link) breaks, and — crucially — it does
// NOT depend on reaching cloudbox. A genuinely-good binary that boots but
// can't reach cloudbox (a real WAN outage, possibly caused by the very
// storm we're guarding against) still self-confirms on uptime, so it is
// never falsely reverted. Only a binary that cannot stay up loses the
// confirmation race, which is precisely the case worth reverting.
//
// Two processes cooperate through the marker file:
//   - the daemon writes the marker after a swap (worker.run), and clears it
//     once it has been up confirmDwell (ArmConfirm) → "confirmed healthy";
//   - the supervisor (the only always-up parent that survives a crash-loop)
//     reads the marker before each respawn and reverts when the new binary
//     has failed to confirm (see cmd/outpost/supervisord_watchdog.go).

var (
	// confirmDwell is how long the new binary must stay up in a single boot
	// to be declared healthy. Var (not const) so tests can shrink it.
	confirmDwell = 3 * time.Minute
	// confirmGraceDeadline is the absolute window from swap to confirmation;
	// past it with the marker still present, the binary never managed to
	// stay up confirmDwell, so it's a revert candidate.
	confirmGraceDeadline = 10 * time.Minute
	// MaxUnconfirmedBoots is how many supervised respawns with the marker
	// still unconfirmed count as a crash-loop (revert candidate) regardless
	// of the deadline. Exported + var so the supervisor watchdog and tests
	// can read/shrink it.
	MaxUnconfirmedBoots = 3
)

// PendingConfirm is the marker an in-flight upgrade leaves on disk so the
// new binary can be confirmed healthy — or reverted if it never is.
type PendingConfirm struct {
	ReleaseID       string    `json:"release_id"`
	FromSHA         string    `json:"from_sha"`    // short commit we upgraded FROM (== <binary>.previous)
	ToSHA           string    `json:"to_sha"`      // short commit we upgraded TO (the new binary)
	PrevPath        string    `json:"prev_path"`   // <binary>.previous, the revert target
	BinaryPath      string    `json:"binary_path"` // live binary
	SwappedAt       time.Time `json:"swapped_at"`
	ConfirmDeadline time.Time `json:"confirm_deadline"`
	BootCount       int       `json:"boot_count"` // supervised respawns observed still-unconfirmed
}

// PendingConfirmPath is where the marker lives — next to the ledger, with a
// name distinct from PendingPath (the manual-mode envelope queue).
func PendingConfirmPath(cacheDir string) string {
	if cacheDir == "" {
		return ""
	}
	return filepath.Join(cacheDir, "upgrade-pending-confirm.json")
}

// WritePendingConfirm atomically writes the marker (temp + rename).
func WritePendingConfirm(path string, pc PendingConfirm) error {
	if path == "" {
		return errors.New("WritePendingConfirm: empty path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(pc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadPendingConfirm returns the marker, or (nil, nil) when there is none.
func ReadPendingConfirm(path string) (*PendingConfirm, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var pc PendingConfirm
	if err := json.Unmarshal(data, &pc); err != nil {
		return nil, err
	}
	return &pc, nil
}

// ClearPendingConfirm removes the marker (best-effort; missing is fine).
func ClearPendingConfirm(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// NewPendingConfirm builds the marker an upgrade leaves after a swap.
func NewPendingConfirm(releaseID, fromSHA, toSHA, binaryPath, prevPath string) PendingConfirm {
	now := time.Now().UTC()
	return PendingConfirm{
		ReleaseID:       releaseID,
		FromSHA:         shortCommit(fromSHA),
		ToSHA:           shortCommit(toSHA),
		PrevPath:        prevPath,
		BinaryPath:      binaryPath,
		SwappedAt:       now,
		ConfirmDeadline: now.Add(confirmGraceDeadline),
	}
}

// ArmConfirm runs in the daemon at startup. If a pending-confirm marker
// exists and names THIS binary (its ToSHA matches our running commit), it
// waits confirmDwell and, if we're still alive, commits — ledgers a
// confirm_ok and clears the marker, declaring the upgrade healthy. If the
// marker instead names the binary we just reverted TO (FromSHA matches), it
// clears the now-stale marker. No marker, or a marker for some other
// commit, is a no-op. Intended to be launched in a goroutine bound to the
// daemon's run context; ctx cancellation (a restart) ends it without
// committing, so a binary that didn't survive the dwell never self-confirms.
func ArmConfirm(ctx context.Context, confirmPath, currentCommit string, ledger *Ledger) {
	pc, err := ReadPendingConfirm(confirmPath)
	if err != nil || pc == nil {
		return
	}
	cur := shortCommit(currentCommit)
	if pc.ToSHA != cur {
		// Not the binary the marker is about. If we are the rollback target
		// (the old binary), the marker is stale — clear it.
		if pc.FromSHA == cur {
			_ = ClearPendingConfirm(confirmPath)
		}
		return
	}
	select {
	case <-ctx.Done():
		return // didn't survive the dwell → leave the marker for the supervisor
	case <-time.After(confirmDwell):
	}
	if ledger != nil {
		_ = ledger.Append(LedgerEntry{
			Step:      "confirm_ok",
			ReleaseID: pc.ReleaseID,
			ToSHA:     pc.ToSHA,
			Detail:    "new binary stayed up; upgrade confirmed healthy",
		})
	}
	if err := ClearPendingConfirm(confirmPath); err != nil {
		slog.Warn("upgrade: clearing confirm marker", "err", err)
	}
}
