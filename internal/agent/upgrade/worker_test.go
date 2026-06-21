package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent"
)

// newWorker is a test helper that wires a Worker with sensible defaults:
// in-memory state controlled by `setState`, a restart counter, and a
// ledger written to a tempdir file.
type harness struct {
	t         *testing.T
	dir       string
	binary    string
	ledger    *Ledger
	worker    *Worker
	restarts  int32
	state     StateSnapshot
	stateMu   sync.Mutex
	srvClient *http.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "outpost")
	// Pre-stage a "current" binary so retainPrevious can hardlink it.
	if err := os.WriteFile(bin, []byte("old binary content"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t:      t,
		dir:    dir,
		binary: bin,
		ledger: NewLedger(filepath.Join(dir, "upgrade.log")),
		state: StateSnapshot{
			UpdateMode:    "auto",
			CurrentCommit: "abc1234",
			BinaryPath:    bin,
			PendingPath:   filepath.Join(dir, "upgrade.pending.json"),
		},
	}
	w, err := NewWorker(Options{
		State:   func() StateSnapshot { h.stateMu.Lock(); defer h.stateMu.Unlock(); return h.state },
		Restart: func() { h.restarts++ },
		Ledger:  h.ledger,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.worker = w
	return h
}

func (h *harness) setState(fn func(*StateSnapshot)) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	fn(&h.state)
}

// serveBinary stands up a TLS server that returns the bytes of `path`.
// Returns the URL, sha256 hex, and the server's http.Client (so the
// worker can validate the test cert).
func (h *harness) serveBinary(path string) (string, string, *http.Client) {
	h.t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	h.t.Cleanup(srv.Close)
	h.srvClient = srv.Client()
	return srv.URL, hex.EncodeToString(sum[:]), srv.Client()
}

func TestWorker_RejectsInvalid(t *testing.T) {
	h := newHarness(t)
	r := h.worker.Apply(context.Background(), Envelope{ReleaseID: "r1"})
	if r.Status != "invalid" {
		t.Fatalf("expected invalid, got %v", r.Status)
	}
}

func TestWorker_RejectsNever(t *testing.T) {
	h := newHarness(t)
	h.setState(func(s *StateSnapshot) { s.UpdateMode = "never" })
	r := h.worker.Apply(context.Background(), Envelope{
		ReleaseID: "r1",
		URL:       "https://example.com/x",
		SHA256:    "deadbeef",
		Commit:    "def5678",
	})
	if r.Status != StatusDisabled {
		t.Fatalf("expected disabled, got %v", r.Status)
	}
	if r.HTTPStatusForTest() != 403 {
		t.Fatalf("expected 403, got %d", r.HTTPStatusForTest())
	}
}

func TestWorker_NeverRefusesEvenWithForce(t *testing.T) {
	h := newHarness(t)
	h.setState(func(s *StateSnapshot) { s.UpdateMode = "never" })
	r := h.worker.Apply(context.Background(), Envelope{
		ReleaseID: "r1",
		URL:       "https://example.com/x",
		SHA256:    "deadbeef",
		Commit:    "def5678",
		Force:     true, // operator-blessed but the host opted out
	})
	if r.Status != StatusDisabled {
		t.Fatalf("expected disabled even with force, got %v", r.Status)
	}
}

func TestWorker_ManualPersistsAndReturnsPending(t *testing.T) {
	h := newHarness(t)
	h.setState(func(s *StateSnapshot) { s.UpdateMode = "manual" })
	env := Envelope{
		ReleaseID: "r-manual",
		URL:       "https://example.com/x",
		SHA256:    "deadbeef",
		Commit:    "def5678",
	}
	r := h.worker.Apply(context.Background(), env)
	if r.Status != StatusPendingManual {
		t.Fatalf("expected pending_manual, got %v: %s", r.Status, r.Detail)
	}
	if r.HTTPStatusForTest() != 202 {
		t.Fatalf("pending_manual should map to 202, got %d", r.HTTPStatusForTest())
	}
	// The pending file must exist and decode back to the same envelope.
	got, err := ReadPending(h.state.PendingPath)
	if err != nil {
		t.Fatalf("read pending: %v", err)
	}
	if got == nil || got.ReleaseID != "r-manual" || got.Commit != "def5678" {
		t.Fatalf("pending mismatch: %+v", got)
	}
	// Ledger should include the pending_manual step.
	entries, _ := h.ledger.Tail(0)
	gotStep := false
	for _, e := range entries {
		if e.Step == "pending_manual" && e.ReleaseID == "r-manual" {
			gotStep = true
		}
	}
	if !gotStep {
		t.Fatalf("expected pending_manual ledger entry: %+v", entries)
	}
}

func TestWorker_RefusesWhenInstalledViaPackageManager(t *testing.T) {
	h := newHarness(t)
	// Marker says brew owns the binary — Apply must refuse with
	// StatusDisabled so brew remains the source of truth for version.
	if err := os.WriteFile(filepath.Join(h.dir, ".outpost-installed-via"), []byte("brew\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := h.worker.Apply(context.Background(), Envelope{
		ReleaseID: "r-pkg",
		URL:       "https://example.com/x",
		SHA256:    "deadbeef",
		Commit:    "def5678",
	})
	if r.Status != StatusDisabled {
		t.Fatalf("expected disabled, got %v: %s", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "brew") {
		t.Fatalf("detail should name the installer: %q", r.Detail)
	}
}

func TestWorker_RefusesPackageManagerEvenWithForce(t *testing.T) {
	h := newHarness(t)
	// Force=true is operator-blessed but must NOT override the marker —
	// drift between cloudbox and the package manager is the failure
	// mode we're guarding against. Removing the marker is the supported
	// override path.
	if err := os.WriteFile(filepath.Join(h.dir, ".outpost-installed-via"), []byte("scoop"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := h.worker.Apply(context.Background(), Envelope{
		ReleaseID: "r-pkg-force",
		URL:       "https://example.com/x",
		SHA256:    "deadbeef",
		Commit:    "def5678",
		Force:     true,
	})
	if r.Status != StatusDisabled {
		t.Fatalf("expected disabled even with force, got %v: %s", r.Status, r.Detail)
	}
}

func TestWorker_AllowsWhenInstalledViaInstaller(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(filepath.Join(h.dir, ".outpost-installed-via"), []byte("installer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Bogus URL → the stage step will fail, but Apply itself should
	// return Accepted because the marker is the right value.
	r := h.worker.Apply(context.Background(), Envelope{
		ReleaseID: "r-installer",
		URL:       "https://localhost-no-server.invalid/x",
		SHA256:    "deadbeef",
		Commit:    "def5678",
	})
	if r.Status != StatusAccepted {
		t.Fatalf("expected accepted (installer marker), got %v: %s", r.Status, r.Detail)
	}
	waitForInFlight(t, h.worker, false, time.Second)
}

func TestWorker_AllowsWhenMarkerMissing(t *testing.T) {
	h := newHarness(t)
	// No marker file written — must allow (backwards-compat for hosts
	// installed before the marker convention).
	r := h.worker.Apply(context.Background(), Envelope{
		ReleaseID: "r-nomarker",
		URL:       "https://localhost-no-server.invalid/x",
		SHA256:    "deadbeef",
		Commit:    "def5678",
	})
	if r.Status != StatusAccepted {
		t.Fatalf("expected accepted (no marker), got %v: %s", r.Status, r.Detail)
	}
	waitForInFlight(t, h.worker, false, time.Second)
}

func TestWorker_RejectsSameCommit(t *testing.T) {
	h := newHarness(t)
	r := h.worker.Apply(context.Background(), Envelope{
		ReleaseID: "r1",
		URL:       "https://example.com/x",
		SHA256:    "deadbeef",
		Commit:    "abc1234", // matches StateSnapshot.CurrentCommit
	})
	if r.Status != StatusSameCommit {
		t.Fatalf("expected same_commit, got %v", r.Status)
	}
}

func TestWorker_RejectsMinFromMismatch(t *testing.T) {
	h := newHarness(t)
	r := h.worker.Apply(context.Background(), Envelope{
		ReleaseID: "r1",
		URL:       "https://example.com/x",
		SHA256:    "deadbeef",
		Commit:    "def5678",
		MinFrom:   "xyz9999", // does not match abc1234
	})
	if r.Status != StatusMinFrom {
		t.Fatalf("expected min_from, got %v", r.Status)
	}
}

func TestWorker_DedupsByReleaseID(t *testing.T) {
	h := newHarness(t)
	// The dedup guard defends the restart window after a SUCCESSFUL swap
	// (v0.7.0): a cloudbox retry of the just-applied release must be a
	// no-op. So the first apply has to actually succeed.
	candidate := fakeOutpostBinary(t, `{"commit":"def56781234","go_version":"go1.26.0"}`, 0)
	url, sha, _ := h.serveBinary(candidate)
	h.worker.client = h.srvClient
	env := Envelope{ReleaseID: "r1", URL: url, SHA256: sha, Commit: "def5678"}

	r1 := h.worker.Apply(context.Background(), env)
	if r1.Status != StatusAccepted {
		t.Fatalf("first call: expected accepted, got %v", r1.Status)
	}
	waitForInFlight(t, h.worker, false, 5*time.Second)
	if h.restarts != 1 {
		t.Fatalf("first apply should have swapped+restarted, got %d restarts", h.restarts)
	}
	// Second call with the same release_id → replay (restart-window defense).
	r2 := h.worker.Apply(context.Background(), env)
	if r2.Status != StatusReplay {
		t.Fatalf("second call after success: expected replay, got %v", r2.Status)
	}
}

// TestWorker_FailedApplyDoesNotDedup is the cross-platform-wedge fix: an
// apply that fails BEFORE the swap (here a probe-rejected candidate — the
// same pre-swap failure path a cross-platform probe_failed takes) must NOT
// leave the replay guard set. Otherwise the puller's later poll for the same
// release_id (with the correct-platform artifact) is StatusReplay'd and the
// host wedges on the old binary until a manual restart — exactly what
// happened to lern on the v0.9.0 rollout.
func TestWorker_FailedApplyDoesNotDedup(t *testing.T) {
	h := newHarness(t)
	// Candidate self-reports a commit different from the envelope → probe
	// rejects it (stand-in for the cross-platform probe_failed).
	candidate := fakeOutpostBinary(t, `{"commit":"99999999999","go_version":"go1.26.0"}`, 0)
	url, sha, _ := h.serveBinary(candidate)
	h.worker.client = h.srvClient
	env := Envelope{ReleaseID: "r-fail", URL: url, SHA256: sha, Commit: "def5678"}

	r1 := h.worker.Apply(context.Background(), env)
	if r1.Status != StatusAccepted {
		t.Fatalf("first call: expected accepted, got %v", r1.Status)
	}
	waitForInFlight(t, h.worker, false, 5*time.Second)
	if h.restarts != 0 {
		t.Fatalf("a probe-failed apply must not restart, got %d", h.restarts)
	}
	// Same release_id again → must NOT dedup (the failed attempt didn't
	// poison the guard), so the puller can re-attempt with a correct binary.
	r2 := h.worker.Apply(context.Background(), env)
	if r2.Status == StatusReplay {
		t.Fatalf("after a FAILED apply the same release_id must not replay-dedup; got replay (regression: poisoned guard)")
	}
	// r2 was Accepted → it spawned a second run goroutine; drain it before
	// the test returns so t.TempDir cleanup doesn't race its candidate write.
	waitForInFlight(t, h.worker, false, 5*time.Second)
}

// failingVerifier is a test ArtifactVerifier that always refuses.
type failingVerifier struct{ msg string }

func (f failingVerifier) Verify(_ Envelope, _ string, _ agent.BuildInfo) error {
	return errors.New(f.msg)
}

func TestWorker_VerifierRejectsCandidate(t *testing.T) {
	h := newHarness(t)
	// Wire a refusing verifier directly into the worker for this test.
	h.worker.verifier = failingVerifier{msg: "signature missing"}

	candidate := fakeOutpostBinary(t, `{"commit":"def56781234","go_version":"go1.26.0"}`, 0)
	url, sha, _ := h.serveBinary(candidate)
	h.worker.client = h.srvClient

	r := h.worker.Apply(context.Background(), Envelope{
		ReleaseID: "r-verify",
		URL:       url,
		SHA256:    sha,
		Commit:    "def5678",
	})
	if r.Status != StatusAccepted {
		t.Fatalf("expected accepted, got %v: %s", r.Status, r.Detail)
	}
	waitForInFlight(t, h.worker, false, 5*time.Second)

	// The verifier refused → swap must NOT have happened.
	if h.restarts != 0 {
		t.Fatalf("expected no restart, got %d", h.restarts)
	}
	live, _ := os.ReadFile(h.binary)
	if string(live) != "old binary content" {
		t.Fatalf("binary was swapped despite verifier refusal: %q", live)
	}

	// Ledger should carry verify_failed entry naming the cause.
	entries, _ := h.ledger.Tail(0)
	gotVerify := false
	for _, e := range entries {
		if e.Step == "verify_failed" && strings.Contains(e.Error, "signature missing") {
			gotVerify = true
		}
	}
	if !gotVerify {
		t.Fatalf("expected verify_failed in ledger: %+v", entries)
	}
}

func TestWorker_HappyPath(t *testing.T) {
	h := newHarness(t)

	// Build a fake outpost binary that self-reports a new commit.
	candidate := fakeOutpostBinary(t, `{"commit":"def56781234","go_version":"go1.26.0"}`, 0)
	url, sha, _ := h.serveBinary(candidate)

	env := Envelope{
		ReleaseID: "r-happy",
		URL:       url,
		SHA256:    sha,
		Commit:    "def5678",
	}

	// Swap in the TLS-aware client so the worker can hit the test server.
	h.worker.client = h.srvClient

	r := h.worker.Apply(context.Background(), env)
	if r.Status != StatusAccepted {
		t.Fatalf("expected accepted, got %v: %s", r.Status, r.Detail)
	}

	waitForInFlight(t, h.worker, false, 5*time.Second)
	if h.restarts != 1 {
		t.Fatalf("expected exactly one restart, got %d", h.restarts)
	}

	// The candidate file should be gone (rename consumed it).
	if _, err := os.Stat(h.binary + ".upgrading"); !os.IsNotExist(err) {
		t.Fatalf("expected candidate consumed, stat err = %v", err)
	}
	// The previous binary should have been retained.
	if _, err := os.Stat(h.binary + ".previous"); err != nil {
		t.Fatalf("expected outpost.previous retained: %v", err)
	}
	// The live binary should now match the fake's content.
	live, _ := os.ReadFile(h.binary)
	orig, _ := os.ReadFile(candidate)
	if len(live) != len(orig) {
		t.Fatalf("binary swap incomplete: live=%d orig=%d", len(live), len(orig))
	}

	// Ledger should have at least received + swap_done.
	entries, err := h.ledger.Tail(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected ≥2 ledger entries, got %d: %+v", len(entries), entries)
	}
	gotSwap := false
	for _, e := range entries {
		if e.Step == "swap_done" {
			gotSwap = true
		}
	}
	if !gotSwap {
		t.Fatalf("expected swap_done in ledger, got %+v", entries)
	}
}

// waitForInFlight blocks until the worker's inFlight bit matches
// `want`, or fails the test after `timeout`. Reaches into the worker
// directly — fine inside the package.
func waitForInFlight(t *testing.T, w *Worker, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		got := w.inFlight
		w.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("inFlight did not reach %v within %v", want, timeout)
}

// HTTPStatusForTest is a Result helper exposed only via build-tag-free
// test reach: we forward to Status.HTTPStatus() so test asserts read
// naturally without re-deriving the mapping.
func (r Result) HTTPStatusForTest() int { return r.Status.HTTPStatus() }

// --- v0.7.0 double-apply regression suite ---------------------------
//
// During the v0.7.0 rollout the fleet fan-out re-applied the upgrade
// on the just-restarted canary host, re-swapping the binary over
// itself and overwriting <binary>.previous (the rollback copy) with
// the new version. Two independent guards both missed:
//   (a) same_commit compared the envelope's full sha against
//       BuildInfo.Short(), which returns the version TAG on release
//       builds — shapes that can never match;
//   (b) the in-memory replay guard died with the pre-swap process —
//       and a successful upgrade always restarts, so the guard was
//       gone exactly when the duplicate envelope arrived.
// These tests pin the fixes: shape-normalized comparisons and a
// ledger-seeded replay guard.

func TestWorker_SameCommitMatchesAcrossShapes(t *testing.T) {
	h := newHarness(t) // CurrentCommit is the short "abc1234"
	// Manual mode keeps the test hermetic if the gate misses: the
	// fall-through lands on pending_manual instead of a network fetch.
	h.setState(func(s *StateSnapshot) { s.UpdateMode = "manual" })
	r := h.worker.Apply(context.Background(), Envelope{
		ReleaseID: "r1",
		URL:       "https://example.com/x",
		SHA256:    "deadbeef",
		Commit:    "abc1234deadbeefdeadbeefdeadbeefdeadbeef0", // full sha, same first 7
	})
	if r.Status != StatusSameCommit {
		t.Fatalf("full-sha envelope against short current commit: expected same_commit, got %v (%s)", r.Status, r.Detail)
	}
}

func TestWorker_MinFromMatchesAcrossShapes(t *testing.T) {
	h := newHarness(t)
	h.setState(func(s *StateSnapshot) { s.UpdateMode = "manual" })
	r := h.worker.Apply(context.Background(), Envelope{
		ReleaseID: "r1",
		URL:       "https://example.com/x",
		SHA256:    "deadbeef",
		Commit:    "def5678",
		MinFrom:   "abc1234deadbeefdeadbeefdeadbeefdeadbeef0", // full sha matching current short
	})
	if r.Status == StatusMinFrom {
		t.Fatalf("min_from refused despite matching current commit: %s", r.Detail)
	}
	if r.Status != StatusPendingManual {
		t.Fatalf("expected pending_manual fall-through, got %v (%s)", r.Status, r.Detail)
	}
}

func TestSeedLastReleaseID(t *testing.T) {
	cases := []struct {
		name  string
		steps []LedgerEntry
		want  string
	}{
		{"empty ledger", nil, ""},
		{"swap_done seeds", []LedgerEntry{
			{ReleaseID: "r1", Step: "received"},
			{ReleaseID: "r1", Step: "swap_done"},
		}, "r1"},
		{"newest swap_done wins", []LedgerEntry{
			{ReleaseID: "r1", Step: "swap_done"},
			{ReleaseID: "r2", Step: "received"},
			{ReleaseID: "r2", Step: "swap_done"},
		}, "r2"},
		{"rollback clears the seed", []LedgerEntry{
			{ReleaseID: "r1", Step: "swap_done"},
			{Step: "rollback"},
		}, ""},
		{"failed attempt does not seed", []LedgerEntry{
			{ReleaseID: "r1", Step: "received"},
			{ReleaseID: "r1", Step: "stage_failed"},
		}, ""},
		{"swap after rollback seeds again", []LedgerEntry{
			{ReleaseID: "r1", Step: "swap_done"},
			{Step: "rollback"},
			{ReleaseID: "r2", Step: "swap_done"},
		}, "r2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLedger(filepath.Join(t.TempDir(), "upgrade.log"))
			for _, e := range tc.steps {
				if err := l.Append(e); err != nil {
					t.Fatal(err)
				}
			}
			if got := seedLastReleaseID(l); got != tc.want {
				t.Fatalf("seed = %q, want %q", got, tc.want)
			}
		})
	}
	if got := seedLastReleaseID(nil); got != "" {
		t.Fatalf("nil ledger seed = %q, want empty", got)
	}
}

func TestNewWorker_ReplayGuardSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	// Simulate the post-upgrade restart: the prior process recorded
	// swap_done for r9, then re-execed — i.e. a brand-new Worker
	// constructed over the same ledger file.
	for _, e := range []LedgerEntry{
		{ReleaseID: "r9", Step: "received"},
		{ReleaseID: "r9", Step: "swap_done"},
	} {
		if err := h.ledger.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	w2, err := NewWorker(Options{
		State:   func() StateSnapshot { h.stateMu.Lock(); defer h.stateMu.Unlock(); return h.state },
		Restart: func() {},
		Ledger:  h.ledger,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := w2.Apply(context.Background(), Envelope{
		ReleaseID: "r9",
		URL:       "https://example.com/x",
		SHA256:    "deadbeef",
		Commit:    "def5678",
	})
	if r.Status != StatusReplay {
		t.Fatalf("duplicate envelope after restart: expected replay, got %v (%s)", r.Status, r.Detail)
	}
}

func TestNewWorker_RollbackClearsReplaySeed(t *testing.T) {
	h := newHarness(t)
	for _, e := range []LedgerEntry{
		{ReleaseID: "r9", Step: "swap_done"},
		{Step: "rollback"},
	} {
		if err := h.ledger.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	h.setState(func(s *StateSnapshot) { s.UpdateMode = "manual" }) // hermetic fall-through
	w2, err := NewWorker(Options{
		State:   func() StateSnapshot { h.stateMu.Lock(); defer h.stateMu.Unlock(); return h.state },
		Restart: func() {},
		Ledger:  h.ledger,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := w2.Apply(context.Background(), Envelope{
		ReleaseID: "r9",
		URL:       "https://example.com/x",
		SHA256:    "deadbeef",
		Commit:    "def5678",
	})
	if r.Status == StatusReplay {
		t.Fatal("rollback should clear the replay seed so the rolled-back release can be re-applied")
	}
	if r.Status != StatusPendingManual {
		t.Fatalf("expected pending_manual, got %v (%s)", r.Status, r.Detail)
	}
}
