package upgrade

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

// LedgerEntry is one JSON line in the upgrade history file. Each
// significant phase of an upgrade (received, stage_start, swap_done,
// restart, failed, rollback_used) emits one of these. The file is
// the source of truth for "what happened to this host" — surfaced
// via `outpost upgrade history` and the outpost://upgrade-history
// MCP resource.
type LedgerEntry struct {
	At        time.Time `json:"at"`
	ReleaseID string    `json:"release_id,omitempty"`
	Step      string    `json:"step"`
	FromSHA   string    `json:"from_sha,omitempty"`
	ToSHA     string    `json:"to_sha,omitempty"`
	URL       string    `json:"url,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// Ledger is an append-only JSONL writer + bounded tail-reader. Path
// is fixed at construction; concurrent appends serialize through mu.
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
// zero, it is filled with the current time. The file is opened
// O_APPEND so concurrent writers from a future fan-out don't have
// to coordinate seek positions — the OS handles atomic byte appends
// on POSIX up to PIPE_BUF, and a single JSON line never exceeds that
// for our shape.
//
// Errors writing the ledger are NOT fatal to the upgrade — we'd
// rather complete an upgrade than abort it because we couldn't
// scribble a record. Callers log Append's error but continue.
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
// that has never been upgraded simply has no history.
//
// Implementation reads the whole file (the ledger is unbounded in
// principle but bounded in practice: one entry per upgrade attempt,
// which is rare enough that even a year of activity stays well under
// a megabyte). When we cross some real threshold we can rotate.
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
	// Default Scanner buffer is 64 KiB. Bump to 1 MiB to cover the
	// occasional fat entry (long URL + long error message).
	buf := make([]byte, 0, 1024)
	scanner.Buffer(buf, 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e LedgerEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // skip malformed lines silently — ledger should be best-effort readable
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
