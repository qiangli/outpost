package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"
)

// Worker walks the configured folders on a schedule (or on demand)
// and writes one Candidate per (folder, fire) to the ledger. The
// scheduler glue lives in manager.go; Worker.RunOnce is the unit of
// work either path invokes.
//
// Concurrency: RunOnce is serialized through inFlight to prevent a
// manual "Run now" overlapping a scheduled fire. Overlap would
// produce two candidates with the same SHA but the second would be
// flagged Skipped — harmless but noisy in the ledger.
type Worker struct {
	ledger *Ledger

	mu       sync.Mutex
	inFlight bool

	// statusMu guards the LastFireAt cache so the admin UI's status
	// render doesn't have to Tail the ledger every poll.
	statusMu   sync.Mutex
	lastFireAt time.Time
}

// NewWorker constructs a Worker writing to the given ledger. A nil
// ledger disables persistence (the worker still computes hashes and
// returns Candidates to callers — useful for tests).
func NewWorker(ledger *Ledger) *Worker {
	return &Worker{ledger: ledger}
}

// LastFireAt returns the UTC timestamp of the most recent RunOnce
// start, or zero if the worker has never fired this process-lifetime.
// Read by the admin UI's status banner.
func (w *Worker) LastFireAt() time.Time {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	return w.lastFireAt
}

// RunOnce iterates folders, picks the newest file from each, computes
// sha256, and appends one Candidate per folder to the ledger. Returns
// the candidates it produced (in folders order) so a manual-fire
// caller can render the result inline without a second ledger Tail.
//
// Errors from individual folders (missing dir, permission, picker
// failure) are recorded as Candidate.Error and do NOT abort the
// remaining folders — one bad folder shouldn't block the rest.
func (w *Worker) RunOnce(ctx context.Context, folders []string) ([]Candidate, error) {
	w.mu.Lock()
	if w.inFlight {
		w.mu.Unlock()
		return nil, errors.New("backup: another run is already in flight")
	}
	w.inFlight = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.inFlight = false
		w.mu.Unlock()
	}()

	now := time.Now().UTC()
	w.statusMu.Lock()
	w.lastFireAt = now
	w.statusMu.Unlock()

	out := make([]Candidate, 0, len(folders))
	for _, folder := range folders {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		default:
		}
		c := w.runFolder(folder, now)
		out = append(out, c)
		if w.ledger != nil {
			_ = w.ledger.Append(c) // ledger errors are non-fatal
		}
	}
	return out, nil
}

// runFolder is the per-folder pipeline: pick → hash → dedup. Always
// returns a Candidate — when something goes wrong, Error is populated
// so the operator can see what failed without grepping logs.
func (w *Worker) runFolder(folder string, now time.Time) Candidate {
	c := Candidate{
		At:     now,
		Folder: folder,
	}
	path, info, err := PickLatest(folder)
	if err != nil {
		if errors.Is(err, ErrNoFiles) {
			c.Error = "no eligible files in folder"
			return c
		}
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			c.Error = fmt.Sprintf("folder missing: %v", err)
			return c
		}
		c.Error = err.Error()
		return c
	}
	c.Path = path
	c.Size = info.Size()
	c.Mtime = info.ModTime().UTC()

	sum, err := hashFile(path)
	if err != nil {
		c.Error = fmt.Sprintf("hash %s: %v", path, err)
		return c
	}
	c.SHA256 = sum

	// Dedup: if the last non-skipped candidate for this folder has
	// the same SHA, this fire is a no-op. The dedup check is best-
	// effort — a ledger read failure marks the candidate as fresh
	// (better to over-report than silently miss).
	if w.ledger != nil {
		if last, err := w.ledger.LastByFolder(folder); err == nil && last.SHA256 == sum {
			c.Skipped = true
		}
	}
	return c
}

// hashFile sha256s the file at path. Streamed via io.Copy so a 4 GiB
// classgo-like archive doesn't pin the whole thing in memory.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
