package backup

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Ledger is the JSONL writer + bounded tail-reader for backup
// Candidates. Shape mirrors internal/agent/upgrade/ledger.go and
// internal/scheduler/ledger.go so a single tail-renderer can read
// any of them later.
type Ledger struct {
	path string
	mu   sync.Mutex
}

func NewLedger(path string) *Ledger { return &Ledger{path: path} }

func (l *Ledger) Path() string { return l.path }

// Append writes one Candidate as a JSON line. Empty path silently
// no-ops (tests / disabled config).
func (l *Ledger) Append(c Candidate) error {
	if l == nil || l.path == "" {
		return nil
	}
	line, err := json.Marshal(c)
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

// Tail returns up to the last `n` candidates, newest last. Missing
// file is empty + nil (a host that has never fired has no history).
func (l *Ledger) Tail(n int) ([]Candidate, error) {
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
	var all []Candidate
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var c Candidate
		if err := json.Unmarshal(line, &c); err != nil {
			continue // skip malformed lines silently
		}
		all = append(all, c)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

// LastByFolder returns the most recent non-skipped candidate for
// folder, or zero if the folder has never produced one. Used by the
// worker's dedup check (skip a fire when the latest picked file's
// sha matches the last shipped one) and by the admin UI to render
// "last backup picked from this folder."
func (l *Ledger) LastByFolder(folder string) (Candidate, error) {
	all, err := l.Tail(0)
	if err != nil {
		return Candidate{}, err
	}
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Folder != folder {
			continue
		}
		if all[i].Skipped {
			continue
		}
		return all[i], nil
	}
	return Candidate{}, nil
}
