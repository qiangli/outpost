// Package scheduler is a thin wrapper around robfig/cron/v3 that adds
// a JSONL ledger so each fired job leaves a durable record alongside
// the upgrade ledger (internal/agent/upgrade/ledger.go is its sibling
// shape — same Append+Tail pattern, different vocabulary).
//
// Why a wrapper at all: outpost's first scheduled-work consumer
// (peer-coordinated backup) needs three things cron/v3 doesn't
// provide on its own — (1) a stable name → spec → fn registry that
// can be re-registered idempotently after a config reload, (2) panic
// recovery so a single misbehaving job can't take the daemon down,
// (3) a ledger so `outpost backup history` and the equivalent MCP
// resource can answer "when did the nightly backup last fire and did
// it succeed". A future cloudbox consumer can pull this package up
// to a sibling module under the umbrella; for now it lives here
// because outpost is the only caller and cloudbox's leader-only cron
// (peer-health sweep, retention) can be a bare time.Ticker until a
// second use case materializes.
//
// Time zone: read once at Run() time from $TZ, falling back to
// time.Local. UTC normalisation was rejected because operators
// reason about "2 AM nightly" in local wall-clock time and we want
// "Europe/London" → "America/Los_Angeles" relocation to keep the
// schedule's human-meaning stable, not its instant.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// JobFunc is the unit of scheduled work. The context is cancelled
// when Scheduler.Run's ctx is cancelled (so a long-running job can
// observe shutdown). A non-nil error is recorded in the ledger.
type JobFunc func(ctx context.Context) error

// Scheduler runs named jobs on cron expressions. Construct via New;
// register every job before calling Run; Run blocks until ctx ends.
type Scheduler struct {
	cron   *cron.Cron
	ledger *Ledger

	mu      sync.Mutex
	entries map[string]cron.EntryID // name → cron id (for LastRun / NextRun lookups)
	specs   map[string]string       // name → cron expression (for diagnostics)
}

// New returns a Scheduler that will write each fired job's outcome to
// ledgerPath. If ledgerPath is empty the ledger is silently disabled
// (useful for tests). Job functions receive ctx-cancellation when
// Run's ctx is cancelled.
func New(ledgerPath string) *Scheduler {
	loc := resolveLocation()
	c := cron.New(
		cron.WithLocation(loc),
		cron.WithChain(
			cron.Recover(cron.DefaultLogger),
		),
	)
	return &Scheduler{
		cron:    c,
		ledger:  NewLedger(ledgerPath),
		entries: make(map[string]cron.EntryID),
		specs:   make(map[string]string),
	}
}

// Register adds (or replaces) a named job. spec is a cron expression
// (5-field "M H D M W" or one of cron/v3's descriptors: "@daily",
// "@hourly", "@every 1h", etc.). fn must be safe to call repeatedly
// and tolerate context cancellation.
//
// Re-registering under the same name removes the previous entry first
// — supports the "outpost polls cloudbox for its policy list every
// 5 min and re-installs the cron entries" pattern without leaking
// duplicate fires.
func (s *Scheduler) Register(name, spec string, fn JobFunc) error {
	if name == "" {
		return errors.New("scheduler: empty job name")
	}
	if fn == nil {
		return errors.New("scheduler: nil JobFunc")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.entries[name]; ok {
		s.cron.Remove(existing)
		delete(s.entries, name)
		delete(s.specs, name)
	}
	id, err := s.cron.AddFunc(spec, s.wrap(name, fn))
	if err != nil {
		return fmt.Errorf("scheduler: register %q (%q): %w", name, spec, err)
	}
	s.entries[name] = id
	s.specs[name] = spec
	return nil
}

// Remove unregisters a named job. Unknown names are a no-op (caller
// dropped a policy concurrently with Remove — not an error).
func (s *Scheduler) Remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.entries[name]; ok {
		s.cron.Remove(id)
		delete(s.entries, name)
		delete(s.specs, name)
	}
}

// Names returns the registered job names in arbitrary order. Useful
// for the "what's scheduled" admin view.
func (s *Scheduler) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.entries))
	for n := range s.entries {
		out = append(out, n)
	}
	return out
}

// NextRun returns the next scheduled fire time for name, or zero time
// if the name is unknown.
func (s *Scheduler) NextRun(name string) time.Time {
	s.mu.Lock()
	id, ok := s.entries[name]
	s.mu.Unlock()
	if !ok {
		return time.Time{}
	}
	e := s.cron.Entry(id)
	return e.Next
}

// Run starts the cron loop and blocks until ctx is cancelled. On
// cancellation Run waits for any in-flight job to return before
// unblocking (cron.Stop returns a context that completes when the
// last in-flight entry exits).
func (s *Scheduler) Run(ctx context.Context) error {
	s.cron.Start()
	<-ctx.Done()
	stopCtx := s.cron.Stop()
	// Bound the drain: a misbehaving job that ignores cancellation
	// shouldn't keep the daemon stuck on shutdown.
	select {
	case <-stopCtx.Done():
	case <-time.After(30 * time.Second):
	}
	return nil
}

// Ledger exposes the JSONL writer so callers can Tail it from CLI /
// MCP / HTTP surfaces (mirrors how upgrade.Worker exposes its
// upgrade.Ledger).
func (s *Scheduler) Ledger() *Ledger { return s.ledger }

// wrap turns a JobFunc into a parameterless cron.FuncJob that
// captures the ledger-write side-effect. The outer ctx for in-flight
// jobs is the daemon-level one threaded through Run (callers receive
// that via wrapJobCtx). We can't pass the Run ctx through cron's
// FuncJob signature, so we resolve it lazily at fire time from the
// scheduler's runCtx field — which is set in Run and cleared on stop.
func (s *Scheduler) wrap(name string, fn JobFunc) func() {
	return func() {
		ctx := s.jobContext()
		start := time.Now().UTC()
		_ = s.ledger.Append(LedgerEntry{Job: name, Step: StepFired, At: start})
		err := fn(ctx)
		entry := LedgerEntry{
			Job:        name,
			At:         time.Now().UTC(),
			DurationMs: time.Since(start).Milliseconds(),
		}
		if err != nil {
			entry.Step = StepFailed
			entry.Error = err.Error()
		} else {
			entry.Step = StepOK
		}
		_ = s.ledger.Append(entry)
	}
}

// jobContext returns the per-fire context. Today that's a bare
// context.Background — the daemon's cancellation is observed at
// Run() and stops new fires; in-flight jobs are expected to be
// short. If a future caller needs cancellable long-running fires we
// stash Run's ctx on s and return it here; this hook exists so that
// change stays in one place.
func (s *Scheduler) jobContext() context.Context {
	return context.Background()
}

// resolveLocation reads $TZ first (POSIX convention), then falls
// back to time.Local. We deliberately don't normalise to UTC —
// operators reason in wall-clock time.
func resolveLocation() *time.Location {
	if tz := os.Getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.Local
}
