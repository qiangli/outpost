package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/qiangli/outpost/internal/agent"
)

// State exposes the running daemon's knowledge that the upgrade
// worker needs to make decisions on each Apply call. Threaded as a
// closure so the worker doesn't have to import admincore/conf and
// pull in their construction graph for tests.
type State func() StateSnapshot

// StateSnapshot is what State returns each tick. The worker treats
// it as a momentary read — re-evaluates on each Apply call so a
// just-flipped update_mode toggle takes effect immediately.
type StateSnapshot struct {
	// UpdateMode is the per-host policy for incoming pushes. One of
	// "auto", "manual", "never" (the conf.UpdateMode* constants;
	// empty is treated as "auto"). See conf/file.go for the contract.
	UpdateMode string
	// CurrentCommit is the running daemon's commit — wire it from
	// agent.ReadBuildInfo().ShortCommit(), NOT Short(): on release
	// builds Short() returns the semver tag ("v0.7.0"), which can
	// never equal an envelope's sha, silently disabling the
	// same-commit and min_from guards. Short or full sha both work;
	// Apply normalizes both sides to 7 chars before comparing.
	CurrentCommit string
	// CurrentDirty reports whether the running binary was built from a
	// working tree with uncommitted changes — i.e. a local developer
	// build carrying code that exists nowhere else. Wire it from
	// agent.ReadBuildInfo().Dirty. Apply refuses cloudbox-pushed
	// upgrades on such a binary: the swap would destroy unreleased work
	// with no way to recover it, since there's no artifact to roll back
	// to. Same reasoning as the installed-via marker below.
	CurrentDirty bool
	// BinaryPath is the live binary's on-disk location (os.Executable
	// of the daemon). The worker stages "<BinaryPath>.upgrading" next
	// to it and hardlinks the current to "<BinaryPath>.previous"
	// before rename for rollback.
	BinaryPath string
	// PendingPath is the path to upgrade.pending.json — where the
	// worker persists envelopes received while in manual mode. The
	// operator's `outpost upgrade apply` reads this file and re-POSTs
	// the envelope with Force=true to consume it.
	PendingPath string
}

// Worker drives the cloudbox-pushed upgrade flow on the daemon side.
// One Worker per daemon process; the route handler routes every
// /admin/upgrade POST through Worker.Apply.
//
// Invariants:
//   - Only one upgrade goroutine runs at a time (enforced by inFlight).
//   - Replays of the same ReleaseID return StatusReplay without doing
//     anything, even after a prior upgrade completed — defends against
//     cloudbox retries during the restart window when the daemon may
//     briefly appear unresponsive.
//   - All state changes funnel through Apply's lock; the worker
//     goroutine never mutates Worker fields after it's spawned (it
//     just appends to the ledger and calls restart).
type Worker struct {
	state    State
	restart  func()
	ledger   *Ledger
	client   *http.Client
	logger   *slog.Logger
	verifier ArtifactVerifier

	// confirmPath is where run() writes the auto-rollback watchdog marker
	// after a swap (see pending_confirm.go). Empty disables the marker
	// (e.g. no cache dir).
	confirmPath string
	// quarantine blocks re-applying a release_id the watchdog auto-reverted
	// on this host. Nil disables the guard.
	quarantine *Quarantine

	mu       sync.Mutex
	inFlight bool
	// lastReleaseID is "most recently accepted-or-completed envelope
	// ID." A second POST with the same ID returns StatusReplay.
	// Seeded from the ledger's newest swap_done entry at construction
	// — the daemon restarts as the final step of every successful
	// upgrade, so a purely in-memory guard forgets exactly when the
	// retry it defends against arrives (observed: the v0.7.0 fleet
	// fan-out re-applied on the canary host seconds after its
	// post-canary restart, re-swapping the binary over itself and
	// overwriting <binary>.previous with the new version).
	lastReleaseID string
}

// Options configures a Worker. State and Restart are required;
// everything else has a sensible zero default.
type Options struct {
	State    State
	Restart  func()
	Ledger   *Ledger
	Client   *http.Client
	Logger   *slog.Logger
	Verifier ArtifactVerifier // nil → NoopVerifier (today's cloudbox-as-root-of-trust)
	// ConfirmPath is where run() writes the auto-rollback watchdog marker
	// after a swap. Empty disables the marker.
	ConfirmPath string
	// QuarantinePath backs the re-apply guard for auto-reverted releases.
	// Empty disables the guard.
	QuarantinePath string
}

// NewWorker constructs a Worker. State and Restart are required —
// without State the worker can't decide anything; without Restart
// the upgrade can't take effect (the daemon would keep running the
// old binary even after the swap).
func NewWorker(opts Options) (*Worker, error) {
	if opts.State == nil {
		return nil, errors.New("upgrade.NewWorker: State is required")
	}
	if opts.Restart == nil {
		return nil, errors.New("upgrade.NewWorker: Restart is required")
	}
	w := &Worker{
		state:       opts.State,
		restart:     opts.Restart,
		ledger:      opts.Ledger,
		client:      opts.Client,
		logger:      opts.Logger,
		verifier:    opts.Verifier,
		confirmPath: opts.ConfirmPath,
	}
	if opts.QuarantinePath != "" {
		w.quarantine = NewQuarantine(opts.QuarantinePath)
	}
	if w.client == nil {
		w.client = http.DefaultClient
	}
	if w.logger == nil {
		w.logger = slog.Default()
	}
	if w.verifier == nil {
		w.verifier = NoopVerifier{}
	}
	w.lastReleaseID = seedLastReleaseID(w.ledger)
	return w, nil
}

// seedLastReleaseID recovers the replay guard across daemon restarts:
// the release_id of the newest swap_done ledger entry, i.e. the
// upgrade this process is (presumably) the result of. A newer
// rollback entry clears the seed — after a rollback the operator
// must be able to re-apply the very release that was rolled back.
// Failed attempts (stage_failed / probe_failed / swap_failed) don't
// seed, so a cloudbox retry after a transient failure + restart is
// still allowed through.
func seedLastReleaseID(l *Ledger) string {
	entries, err := l.Tail(100)
	if err != nil {
		return ""
	}
	for i := len(entries) - 1; i >= 0; i-- {
		switch entries[i].Step {
		case "rollback":
			return ""
		case "swap_done":
			return entries[i].ReleaseID
		}
	}
	return ""
}

// Apply is the single entry point. The route handler calls this
// after binding the JSON body. Returns the wire Result; the handler
// maps Status → HTTP code via HTTPStatus.
func (w *Worker) Apply(ctx context.Context, env Envelope) Result {
	if err := env.Validate(); err != nil {
		return Result{Status: "invalid", Detail: err.Error(), ReleaseID: env.ReleaseID}
	}

	// Quarantine short-circuits BOTH the push and the pull path: a release
	// the watchdog auto-reverted on this host must not be re-applied (the
	// puller would otherwise re-pull /fleet/target, re-brick, re-revert in a
	// slow flap). Cleared only by an operator or a newer release_id.
	if w.quarantine != nil && w.quarantine.Has(env.ReleaseID) {
		return Result{Status: StatusQuarantined, Detail: "release was auto-reverted on this host; cleared by operator (`outpost upgrade unquarantine`) or a newer release", ReleaseID: env.ReleaseID, Commit: env.Commit}
	}

	w.mu.Lock()
	if w.inFlight {
		w.mu.Unlock()
		return Result{Status: StatusInFlight, Detail: "another upgrade is currently running", ReleaseID: env.ReleaseID}
	}
	if env.ReleaseID == w.lastReleaseID {
		w.mu.Unlock()
		return Result{Status: StatusReplay, Detail: "release_id already handled this run", ReleaseID: env.ReleaseID, Commit: env.Commit}
	}

	st := w.state()
	// "never" beats everything (including Force=true) — operators
	// who want a frozen host shouldn't be overridden by a cloudbox-
	// side button. Flip the mode first.
	if st.UpdateMode == "never" {
		w.mu.Unlock()
		return Result{Status: StatusDisabled, Detail: "update_mode is 'never'; operator must change it to accept cloudbox-pushed upgrades", ReleaseID: env.ReleaseID}
	}
	// Dirty build: the running binary was compiled from a working tree
	// with uncommitted changes, so it carries code that exists in no
	// release and no git object. Swapping it destroys that work
	// irrecoverably — .previous would hold the same dirty build only
	// until the next upgrade, and nothing can rebuild it. Refuse, and
	// say how to proceed. Force=true does NOT bypass this, same
	// precedent as never-mode and the installed-via marker: the
	// operator's escape hatch is to install a real build (commit and
	// release it, or `outpost upgrade --local`), not a flag.
	if st.CurrentDirty {
		w.mu.Unlock()
		return Result{
			Status:    StatusDisabled,
			Detail:    "running a dirty local build (uncommitted changes); refusing to overwrite unreleased work — commit and release, or install a clean build first",
			ReleaseID: env.ReleaseID,
		}
	}
	// installed-via marker: when a package manager owns this binary
	// (brew, scoop, apt, …) the cloudbox-pushed upgrade would race
	// the package manager's record of "what version is installed",
	// and the next `brew upgrade` / `scoop update` could undo us.
	// Defer to the owning installer. "installer" (from install.sh /
	// install.ps1) and "manual" (operator hand-placed) are allowed;
	// no marker is also allowed (backwards-compat for hosts that
	// pre-date the marker convention). Force=true does NOT bypass
	// this — same precedent as never mode. Operator override is
	// removing the marker file.
	if via, _ := installedVia(st.BinaryPath); via != "" && via != "installer" && via != "manual" {
		w.mu.Unlock()
		return Result{
			Status:    StatusDisabled,
			Detail:    fmt.Sprintf("installed via %q — use that package manager to upgrade (remove .outpost-installed-via next to the binary to override)", via),
			ReleaseID: env.ReleaseID,
		}
	}
	// Normalize both sides to short commit, same as Probe: envelopes
	// legitimately carry either shape (short from the CLI, full
	// 40-char from the GH-Action release webhook).
	if st.CurrentCommit != "" && shortCommit(env.Commit) == shortCommit(st.CurrentCommit) {
		w.mu.Unlock()
		return Result{Status: StatusSameCommit, Detail: "daemon is already at " + env.Commit, ReleaseID: env.ReleaseID, Commit: env.Commit}
	}
	if env.MinFrom != "" && st.CurrentCommit != "" && shortCommit(env.MinFrom) != shortCommit(st.CurrentCommit) {
		// MinFrom is conservative: only the exact match is acceptable.
		// We don't have an ordering between arbitrary git commits,
		// and cloudbox already knows the fleet's commit distribution
		// — it can choose to dispatch only to hosts at the right
		// MinFrom commit, or omit MinFrom for unconditional upgrades.
		w.mu.Unlock()
		return Result{Status: StatusMinFrom, Detail: fmt.Sprintf("daemon at %s, envelope requires from %s", st.CurrentCommit, env.MinFrom), ReleaseID: env.ReleaseID}
	}

	// Manual mode: persist the envelope and return without staging.
	// Force=true bypasses the manual gate (used by `outpost upgrade
	// apply` + cloudbox's "Apply" UI button), in which case we fall
	// through to the auto path.
	if st.UpdateMode == "manual" && !env.Force {
		w.mu.Unlock()
		if err := writePendingEnvelope(st.PendingPath, env); err != nil {
			w.logger.Warn("manual: persist pending envelope", "err", err)
		}
		_ = w.appendLedger(LedgerEntry{
			ReleaseID: env.ReleaseID,
			Step:      "pending_manual",
			FromSHA:   st.CurrentCommit,
			ToSHA:     env.Commit,
			URL:       env.URL,
			Detail:    "queued; awaiting operator apply",
		})
		return Result{Status: StatusPendingManual, Detail: "update queued; operator must run `outpost upgrade apply` or click Apply in cloudbox UI", ReleaseID: env.ReleaseID, Commit: env.Commit}
	}

	// Remember the prior guard value so a run that FAILS before the swap
	// can restore it (see run's defer). Setting lastReleaseID now keeps a
	// concurrent retry during the run on the inFlight (409) path; only a
	// successful swap makes it stick.
	prevReleaseID := w.lastReleaseID
	w.inFlight = true
	w.lastReleaseID = env.ReleaseID
	binaryPath := st.BinaryPath
	pendingPath := st.PendingPath
	w.mu.Unlock()

	if binaryPath == "" {
		w.mu.Lock()
		w.inFlight = false
		w.lastReleaseID = prevReleaseID // never started; un-poison the guard
		w.mu.Unlock()
		return Result{Status: "invalid", Detail: "daemon has no binary_path; cannot stage candidate", ReleaseID: env.ReleaseID}
	}

	_ = w.appendLedger(LedgerEntry{
		ReleaseID: env.ReleaseID,
		Step:      "received",
		FromSHA:   st.CurrentCommit,
		ToSHA:     env.Commit,
		URL:       env.URL,
	})

	go w.run(context.WithoutCancel(ctx), env, binaryPath, st.CurrentCommit, pendingPath, prevReleaseID)
	return Result{Status: StatusAccepted, Detail: "staging candidate", ReleaseID: env.ReleaseID, Commit: env.Commit}
}

// run owns the heavy lifting: stage → probe → link-previous → rename
// → restart. Each phase emits one ledger entry. Errors abort the
// flow but never escape (we're the only goroutine reading our state).
// The defer guarantees inFlight clears even if we panic.
//
// pendingPath, when non-empty, is the upgrade.pending.json this run
// is consuming — if Force was true and there's a pending envelope
// for this release_id, the file is removed on success so the next
// "Apply" UI click doesn't see a stale entry.
func (w *Worker) run(ctx context.Context, env Envelope, binaryPath, fromSHA, pendingPath, prevReleaseID string) {
	swapped := false
	defer func() {
		w.mu.Lock()
		w.inFlight = false
		if !swapped {
			// Failed before the swap (stage/probe/verify/swap error — e.g. a
			// cross-platform candidate that probe-rejected). Restore the
			// replay guard so the puller can retry the SAME release_id with
			// the correct-platform artifact, instead of being StatusReplay'd
			// and wedged on the old binary until a daemon restart.
			w.lastReleaseID = prevReleaseID
		}
		w.mu.Unlock()
	}()

	candidate := binaryPath + ".upgrading"
	// Pre-clean: a candidate file from a crashed prior attempt would
	// fail O_EXCL. Defense in depth over the route-handler's checks.
	_ = os.Remove(candidate)

	if err := StageFromURL(ctx, candidate, env.URL, env.SHA256, w.client); err != nil {
		_ = os.Remove(candidate)
		w.fail(env, "stage_failed", fromSHA, err)
		return
	}

	build, err := Probe(candidate, env.Commit)
	if err != nil {
		_ = os.Remove(candidate)
		w.fail(env, "probe_failed", fromSHA, err)
		return
	}

	if err := w.verifier.Verify(env, candidate, build); err != nil {
		_ = os.Remove(candidate)
		w.fail(env, "verify_failed", fromSHA, err)
		return
	}

	previous := binaryPath + ".previous"
	retained := true
	if err := RetainPrevious(binaryPath, previous); err != nil {
		// Rollback won't be available for this upgrade — but the
		// upgrade itself can still proceed. The ledger records why.
		retained = false
		_ = w.appendLedger(LedgerEntry{
			ReleaseID: env.ReleaseID,
			Step:      "previous_unavailable",
			FromSHA:   fromSHA,
			Error:     err.Error(),
		})
	}

	if err := SwapAtomic(binaryPath, candidate); err != nil {
		_ = os.Remove(candidate)
		w.fail(env, "swap_failed", fromSHA, err)
		return
	}
	// The swap landed — this release IS now applied, so the replay guard
	// (set in Apply) must STICK to defend the restart window (v0.7.0). The
	// defer above only restores it on a pre-swap failure.
	swapped = true

	_ = w.appendLedger(LedgerEntry{
		ReleaseID: env.ReleaseID,
		Step:      "swap_done",
		FromSHA:   fromSHA,
		ToSHA:     agent.BuildInfo{Commit: build.Commit, Dirty: build.Dirty}.Short(),
		Detail:    "binary swapped; scheduling restart",
	})

	// Arm the auto-rollback watchdog: leave a marker the new binary must
	// confirm (by staying up — see pending_confirm.go) or the supervisor
	// reverts. Only when we actually retained a .previous to revert to —
	// no rollback target ⇒ no watchdog. Marker write is best-effort; a
	// failure just means no auto-rollback for this upgrade.
	if retained && w.confirmPath != "" {
		if err := WritePendingConfirm(w.confirmPath,
			NewPendingConfirm(env.ReleaseID, fromSHA, build.Commit, binaryPath, previous)); err != nil {
			w.logger.Warn("upgrade: writing confirm marker", "err", err)
		}
	}

	// Consume the pending file if this run was Force-driven and the
	// release_id matches what was queued. Mismatches leave the file
	// alone — a manual host might have a different pending update
	// that an admin shouldn't see clobbered by an unrelated cloudbox
	// push that happened to come through with Force=true.
	if env.Force && pendingPath != "" {
		if pending, _ := readPendingEnvelope(pendingPath); pending != nil && pending.ReleaseID == env.ReleaseID {
			_ = os.Remove(pendingPath)
		}
	}

	w.restart()
	// After this point the daemon is on its way out. Any further
	// ledger writes risk being interrupted mid-flush — the
	// "swap_done" entry above is the last reliable record, and it's
	// enough for cloudbox to confirm landing (the post-restart
	// status push will carry the new sha).
}

// RetainPrevious snapshots the running binary at "<binary>.previous"
// before rename. Hardlink first (instant, single-fs); fall back to a
// copy on cross-fs or filesystems without hardlink support. Exported
// so the CLI's `outpost upgrade --local|--from` path can call it
// before its own swap — both paths leave a rollback target on disk.
func RetainPrevious(binary, previous string) error {
	// Drop any older "previous" — we only keep one generation back.
	// More than that would balloon disk usage for noisy upgrade
	// cycles; rollback to N-2 is not a use case we're solving.
	_ = os.Remove(previous)

	if err := os.Link(binary, previous); err == nil {
		return nil
	}

	src, err := os.Open(binary)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(previous, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		_ = os.Remove(previous)
		return err
	}
	return nil
}

// fail emits a ledger entry for an upgrade that died mid-flow. The
// step name is the phase that failed (stage_failed / probe_failed /
// swap_failed); err carries the precise reason.
func (w *Worker) fail(env Envelope, step, fromSHA string, cause error) {
	w.logger.Error("upgrade failed", "release_id", env.ReleaseID, "step", step, "err", cause)
	_ = w.appendLedger(LedgerEntry{
		ReleaseID: env.ReleaseID,
		Step:      step,
		FromSHA:   fromSHA,
		ToSHA:     env.Commit,
		URL:       env.URL,
		Error:     cause.Error(),
	})
}

// installedVia reads the marker file (".outpost-installed-via") next
// to binaryPath. Returns the trimmed lowercased content, or "" if the
// file is missing or binaryPath is empty. I/O errors other than
// not-exist surface to the caller; in Apply() we ignore them (the
// marker is advisory — a transient read failure shouldn't block an
// upgrade that's otherwise valid).
func installedVia(binaryPath string) (string, error) {
	if binaryPath == "" {
		return "", nil
	}
	p := filepath.Join(filepath.Dir(binaryPath), ".outpost-installed-via")
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(string(data))), nil
}

func (w *Worker) appendLedger(e LedgerEntry) error {
	if w.ledger == nil {
		return nil
	}
	if err := w.ledger.Append(e); err != nil {
		w.logger.Warn("ledger append", "err", err)
		return err
	}
	return nil
}
