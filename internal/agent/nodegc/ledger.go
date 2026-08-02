package nodegc

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Ledger events. Deletes and refusals both get a durable line: this
// collector destroys cluster state unattended, so "what did it do and
// why" must survive the daemon's own logs (which rotate, and which a
// self-restart truncates from view). Same precedent as the upgrade
// subsystem's upgrade.log.
const (
	// EventDeleted records one real node deletion. These entries are
	// also the restart-safe rate limiter: the delete budget counts
	// EventDeleted lines inside the trailing window, so a daemon that
	// restarts ten times in ten minutes shares ONE budget instead of
	// minting ten.
	EventDeleted = "deleted"
	// EventDryRunDeleted records a node dry-run mode WOULD have
	// deleted. Never counted against the real budget.
	EventDryRunDeleted = "dry_run_deleted"
	// EventRefusedMassStale records a whole pass refused by the
	// cluster-wide unhealthy circuit breaker.
	EventRefusedMassStale = "refused_mass_stale"
	// EventRefusedClockSkew records a whole pass refused because a
	// node's readiness evidence was timestamped in our future.
	EventRefusedClockSkew = "refused_clock_skew"
)

// LedgerEntry is one JSON line in the nodegc history file.
type LedgerEntry struct {
	At            time.Time `json:"at"`
	Event         string    `json:"event"`
	Node          string    `json:"node,omitempty"`
	UID           string    `json:"uid,omitempty"`
	NotReadySince string    `json:"not_ready_since,omitempty"`
	Stale         int       `json:"stale,omitempty"`
	Scope         int       `json:"scope,omitempty"`
	Detail        string    `json:"detail,omitempty"`
}

// Ledger is an append-only JSONL writer at a fixed path, mirroring
// internal/agent/upgrade.Ledger. A nil Ledger (or empty path) is a
// valid no-op sink — but note Collector treats "no ledger" as "no
// durable budget", falling back to in-process memory only.
//
// Single-writer by construction: exactly one outpost daemon runs per
// host (the pidfile gate in `start`), and only the control-plane
// host's collector writes this file — so O_APPEND line writes are
// sufficient and no inter-process lock is needed here (contrast the
// CNI IPAM ledger, which IS written by concurrent processes).
type Ledger struct {
	path string
	mu   sync.Mutex
}

// NewLedger returns a Ledger backed by path. Doesn't touch the
// filesystem until the first Append.
func NewLedger(path string) *Ledger {
	return &Ledger{path: path}
}

// Path is exposed so callers can include it in diagnostics.
func (l *Ledger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Append writes one entry as a single JSON line. A zero At is filled
// with the current time. Append errors on REFUSAL entries are logged
// by the caller and never block anything; a failure to record a
// DELETE is treated as fatal to the pass by the caller (Collector
// refuses to keep deleting when it cannot durably count deletions —
// the record is what bounds the rate across restarts).
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

// CountDeletesSince returns how many real deletions (EventDeleted)
// the ledger records strictly after cutoff. A missing file is zero
// deletions, not an error. Malformed lines are skipped — a corrupt
// line can therefore UNDER-count by at most the lines lost, which is
// bounded by MaxDeletes per window and accepted; any other read
// error is returned and the caller must fail safe (refuse deletes).
func (l *Ledger) CountDeletesSince(cutoff time.Time) (int, error) {
	if l == nil || l.path == "" {
		return 0, nil
	}
	f, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	n := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e LedgerEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if e.Event == EventDeleted && e.At.After(cutoff) {
			n++
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

// Tail returns up to the last n entries, newest last. Missing file
// returns an empty slice. Mirrors upgrade.Ledger.Tail for a future
// `outpost cluster nodegc history` surface.
func (l *Ledger) Tail(n int) ([]LedgerEntry, error) {
	if l == nil || l.path == "" || n <= 0 {
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
	scanner.Buffer(make([]byte, 0, 1024), 1<<20)
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
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}
