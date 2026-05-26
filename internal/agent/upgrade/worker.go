package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
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
// just-flipped AutoUpgrade toggle takes effect immediately.
type StateSnapshot struct {
	// AutoUpgrade reflects fc.AutoUpgradeOn(). When false, the
	// worker returns StatusDisabled without staging anything.
	AutoUpgrade bool
	// CurrentCommit is the running daemon's short commit (e.g.
	// "820e2e1"). Used for the same-commit short-circuit and the
	// min_from precondition.
	CurrentCommit string
	// BinaryPath is the live binary's on-disk location (os.Executable
	// of the daemon). The worker stages "<BinaryPath>.upgrading" next
	// to it and hardlinks the current to "<BinaryPath>.previous"
	// before rename for rollback.
	BinaryPath string
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

	mu       sync.Mutex
	inFlight bool
	// lastReleaseID is "most recently accepted-or-completed envelope
	// ID." A second POST with the same ID returns StatusReplay.
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
		state:    opts.State,
		restart:  opts.Restart,
		ledger:   opts.Ledger,
		client:   opts.Client,
		logger:   opts.Logger,
		verifier: opts.Verifier,
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
	return w, nil
}

// Apply is the single entry point. The route handler calls this
// after binding the JSON body. Returns the wire Result; the handler
// maps Status → HTTP code via HTTPStatus.
func (w *Worker) Apply(ctx context.Context, env Envelope) Result {
	if err := env.Validate(); err != nil {
		return Result{Status: "invalid", Detail: err.Error(), ReleaseID: env.ReleaseID}
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
	if !st.AutoUpgrade {
		w.mu.Unlock()
		// No ledger entry for disabled rejections — they're a steady-
		// state condition, not an event. The operator flips the
		// toggle elsewhere; logging every cloudbox poll is noise.
		return Result{Status: StatusDisabled, Detail: "auto_upgrade is off; operator must enable to accept cloudbox-pushed upgrades", ReleaseID: env.ReleaseID}
	}
	if st.CurrentCommit != "" && env.Commit == st.CurrentCommit {
		w.mu.Unlock()
		return Result{Status: StatusSameCommit, Detail: "daemon is already at " + env.Commit, ReleaseID: env.ReleaseID, Commit: env.Commit}
	}
	if env.MinFrom != "" && st.CurrentCommit != "" && env.MinFrom != st.CurrentCommit {
		// MinFrom is conservative: only the exact match is acceptable.
		// We don't have an ordering between arbitrary git commits,
		// and cloudbox already knows the fleet's commit distribution
		// — it can choose to dispatch only to hosts at the right
		// MinFrom commit, or omit MinFrom for unconditional upgrades.
		w.mu.Unlock()
		return Result{Status: StatusMinFrom, Detail: fmt.Sprintf("daemon at %s, envelope requires from %s", st.CurrentCommit, env.MinFrom), ReleaseID: env.ReleaseID}
	}

	w.inFlight = true
	w.lastReleaseID = env.ReleaseID
	binaryPath := st.BinaryPath
	w.mu.Unlock()

	if binaryPath == "" {
		w.mu.Lock()
		w.inFlight = false
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

	go w.run(context.WithoutCancel(ctx), env, binaryPath, st.CurrentCommit)
	return Result{Status: StatusAccepted, Detail: "staging candidate", ReleaseID: env.ReleaseID, Commit: env.Commit}
}

// run owns the heavy lifting: stage → probe → link-previous → rename
// → restart. Each phase emits one ledger entry. Errors abort the
// flow but never escape (we're the only goroutine reading our state).
// The defer guarantees inFlight clears even if we panic.
func (w *Worker) run(ctx context.Context, env Envelope, binaryPath, fromSHA string) {
	defer func() {
		w.mu.Lock()
		w.inFlight = false
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
	if err := retainPrevious(binaryPath, previous); err != nil {
		// Rollback won't be available for this upgrade — but the
		// upgrade itself can still proceed. The ledger records why.
		_ = w.appendLedger(LedgerEntry{
			ReleaseID: env.ReleaseID,
			Step:      "previous_unavailable",
			FromSHA:   fromSHA,
			Error:     err.Error(),
		})
	}

	if err := os.Rename(candidate, binaryPath); err != nil {
		_ = os.Remove(candidate)
		w.fail(env, "swap_failed", fromSHA, err)
		return
	}

	_ = w.appendLedger(LedgerEntry{
		ReleaseID: env.ReleaseID,
		Step:      "swap_done",
		FromSHA:   fromSHA,
		ToSHA:     agent.BuildInfo{Commit: build.Commit, Dirty: build.Dirty}.Short(),
		Detail:    "binary swapped; scheduling restart",
	})

	w.restart()
	// After this point the daemon is on its way out. Any further
	// ledger writes risk being interrupted mid-flush — the
	// "swap_done" entry above is the last reliable record, and it's
	// enough for cloudbox to confirm landing (the post-restart
	// status push will carry the new sha).
}

// retainPrevious snapshots the running binary at "<binary>.previous"
// before rename. Hardlink first (instant, single-fs); fall back to a
// copy on cross-fs or filesystems without hardlink support.
func retainPrevious(binary, previous string) error {
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

