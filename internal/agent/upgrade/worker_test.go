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
	t          *testing.T
	dir        string
	binary     string
	ledger     *Ledger
	worker     *Worker
	restarts   int32
	state      StateSnapshot
	stateMu    sync.Mutex
	srvClient  *http.Client
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
	// First call passes validation and gets to "accepted" (the stage
	// will subsequently fail on the bogus URL, but the dedup tracking
	// happens at Apply time, before that).
	env := Envelope{
		ReleaseID: "r1",
		URL:       "https://localhost-no-server.invalid/x",
		SHA256:    "deadbeef",
		Commit:    "def5678",
	}
	r1 := h.worker.Apply(context.Background(), env)
	if r1.Status != StatusAccepted {
		t.Fatalf("first call: expected accepted, got %v", r1.Status)
	}
	// Wait for the goroutine to finish so inFlight clears; then the
	// second call should hit the dedup branch (replay), not in_flight.
	waitForInFlight(t, h.worker, false, time.Second)
	r2 := h.worker.Apply(context.Background(), env)
	if r2.Status != StatusReplay {
		t.Fatalf("second call: expected replay, got %v", r2.Status)
	}
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
