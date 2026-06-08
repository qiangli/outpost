package scheduler

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Step names emitted by the wrap-fn in scheduler.go. Kept as
// constants so a CLI history-renderer doesn't string-match against
// loose literals.
const (
	StepFired  = "fired"
	StepOK     = "ok"
	StepFailed = "failed"
)

// LedgerEntry is one JSONL line in the scheduler history file.
// Shape intentionally parallels internal/agent/upgrade.LedgerEntry
// so a single tail-renderer can handle both files later (the upgrade
// surface uses ReleaseID where the scheduler uses Job).
type LedgerEntry struct {
	At         time.Time `json:"at"`
	Job        string    `json:"job,omitempty"`
	Step       string    `json:"step"`
	DurationMs int64     `json:"duration_ms,omitempty"`
	Error      string    `json:"error,omitempty"`
	Detail     string    `json:"detail,omitempty"`
}

// Ledger is an append-only JSONL writer + bounded tail-reader. Path
// is fixed at construction; concurrent appends serialize through mu.
// An empty path disables writes silently (returns nil from Append).
type Ledger struct {
	path string
	mu   sync.Mutex
}

// NewLedger returns a Ledger backed by `path`. Doesn't touch the
// filesystem until the first Append.
func NewLedger(path string) *Ledger {
	return &Ledger{path: path}
}

// Path is exposed so callers can include it in diagnostics.
func (l *Ledger) Path() string { return l.path }

// Append writes one entry as a single JSON line. If `entry.At` is
// zero, it is filled with the current UTC time.
//
// Errors writing the ledger are NOT fatal to the caller's flow — we
// would rather complete the scheduled job than abort it because we
// couldn't scribble a record. Callers can log Append's error but
// should continue.
func (l *Ledger) Append(entry LedgerEntry) error {
	if l == nil || l.path == "" {
		return nil
	}
	if entry.At.IsZero() {
		entry.At = time.Now().UTC()
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

// Tail returns up to the last `n` ledger entries, newest last. A
// missing ledger file returns an empty slice without error — a host
// that has never fired a scheduled job has no history.
func (l *Ledger) Tail(n int) ([]LedgerEntry, error) {
	if l == nil || l.path == "" {
		return nil, nil
	}
	f, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var all []LedgerEntry
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024)
	scanner.Buffer(buf, 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e LedgerEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		all = append(all, e)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

// LastByJob returns the most recent ledger entry for `job`, or a
// zero entry + nil error if the job has never fired. Used by status
// surfaces to render "last ran at, succeeded/failed".
func (l *Ledger) LastByJob(job string) (LedgerEntry, error) {
	all, err := l.Tail(0)
	if err != nil {
		return LedgerEntry{}, err
	}
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Job == job && (all[i].Step == StepOK || all[i].Step == StepFailed) {
			return all[i], nil
		}
	}
	return LedgerEntry{}, nil
}
