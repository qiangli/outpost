// Reachability ledger: append-only JSONL of every successful peer
// dial. Wave 3B.1 just records; Wave 3B.2 (Memberlist gossip + the
// route-to hint surface) consumes the ledger to build transitive
// reachability hints.
//
// Storage shape mirrors PRoPHET-style DTN routing: each entry is a
// fact "Self reached Peer via Endpoint at Time with Latency." The set
// is grow-only with size-bounded rotation — when the file exceeds
// LedgerMaxEntries, the oldest LedgerMaxEntries/2 entries are pruned.
// That gives us a rolling window of recent contacts without ever
// having to do a full sort.
//
// Operator-debuggable: it's plain JSONL, one entry per line. `tail`
// works; `jq` works. No binary encoding to fight.
package discovery

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LedgerMaxEntries is the soft cap on the ledger file. After this many
// entries are appended, rotation prunes the oldest half. Sized so
// 5000 entries kept ≈ 200 days of two-edge-per-day observations
// (typical home-host pattern).
const LedgerMaxEntries = 10000

// Ledger is the persistent append-only log of ReachabilityEdges.
// Safe for concurrent use; one global instance per daemon process
// (callers reach it via OpenLedger).
type Ledger struct {
	path string

	mu      sync.Mutex
	entries int // approximate; resynced on rotation
}

// OpenLedger constructs a Ledger at the given path. Creates the
// containing directory (mode 0700) on demand. Returns an empty Ledger
// even when the file doesn't exist yet — first Append creates it.
func OpenLedger(path string) (*Ledger, error) {
	if path == "" {
		return nil, errors.New("discovery: empty ledger path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir ledger parent: %w", err)
	}
	l := &Ledger{path: path}
	// Count existing entries so the rotation threshold reflects on-
	// disk reality after a daemon restart.
	if n, err := l.countLines(); err == nil {
		l.entries = n
	}
	return l, nil
}

// Append writes one edge to the ledger. Errors during write are
// reported but the daemon should NOT abort on them — a missing
// ledger entry is far less bad than a daemon crash. Callers
// typically log-and-continue.
func (l *Ledger) Append(e ReachabilityEdge) error {
	if l == nil {
		return nil
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal ledger entry: %w", err)
	}
	b = append(b, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open ledger %s: %w", l.path, err)
	}
	if _, werr := f.Write(b); werr != nil {
		_ = f.Close()
		return fmt.Errorf("append ledger: %w", werr)
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("close ledger: %w", cerr)
	}
	l.entries++
	if l.entries >= LedgerMaxEntries {
		// Prune older half; tolerate errors (still better to have the
		// daemon up than to crash on a housekeeping failure).
		_ = l.rotateLocked()
	}
	return nil
}

// Tail returns the most-recent n entries, newest-last. n == 0 means
// "all". Cheap for n up to a few thousand on rotated ledgers.
func (l *Ledger) Tail(n int) ([]ReachabilityEdge, error) {
	if l == nil {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return []ReachabilityEdge{}, nil
		}
		return nil, err
	}
	defer f.Close()
	all := make([]ReachabilityEdge, 0, 256)
	sc := bufio.NewScanner(f)
	// Bumped from default 64K so a long endpoint hostname doesn't
	// break parsing; ledger lines stay well under 4K.
	sc.Buffer(make([]byte, 0, 16*1024), 128*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e ReachabilityEdge
		if err := json.Unmarshal(line, &e); err != nil {
			// Skip corrupted lines silently — a partial write at
			// crash time shouldn't poison the whole tail.
			continue
		}
		all = append(all, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

// countLines is the cheap on-disk size check used to set the initial
// `entries` count at OpenLedger time.
func (l *Ledger) countLines() (int, error) {
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 16*1024), 128*1024)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			n++
		}
	}
	return n, sc.Err()
}

// rotateLocked prunes the oldest half of the ledger. Caller must
// hold l.mu. Atomic via tmp + rename so a crash mid-rotation leaves
// the original file intact.
func (l *Ledger) rotateLocked() error {
	all := make([]ReachabilityEdge, 0, l.entries)
	f, err := os.Open(l.path)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 16*1024), 128*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e ReachabilityEdge
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		all = append(all, e)
	}
	_ = sc.Err() // rotation is best-effort; corrupted input was already skipped
	_ = f.Close()
	if len(all) <= LedgerMaxEntries/2 {
		// Nothing to prune; reset our count and bail.
		l.entries = len(all)
		return nil
	}
	keep := all[len(all)-LedgerMaxEntries/2:]
	tmp := l.path + ".tmp"
	wf, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	for _, e := range keep {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		b = append(b, '\n')
		if _, err := wf.Write(b); err != nil {
			_ = wf.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := wf.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return err
	}
	l.entries = len(keep)
	return nil
}
