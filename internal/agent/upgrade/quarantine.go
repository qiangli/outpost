package upgrade

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Quarantine records release_ids that were auto-reverted on this host, so the
// puller doesn't immediately re-pull and re-brick the same bad release in a
// slow flap. It is the cross-process channel between the supervisor (which
// adds an entry at revert time) and the daemon's Worker.Apply (which refuses
// a quarantined release). Cleared only by an operator (`outpost upgrade
// unquarantine`) or superseded by cloudbox publishing a different release_id.
type Quarantine struct {
	path string
	mu   sync.Mutex
}

// QuarantineEntry is one quarantined release, with provenance for the
// operator who inspects `outpost upgrade history` / the unquarantine CLI.
type QuarantineEntry struct {
	ReleaseID  string    `json:"release_id"`
	Commit     string    `json:"commit,omitempty"`
	RevertedAt time.Time `json:"reverted_at"`
	Reason     string    `json:"reason,omitempty"`
}

type quarantineFile struct {
	Releases map[string]QuarantineEntry `json:"releases"`
}

// QuarantinePath is where the quarantine set lives — next to the ledger.
func QuarantinePath(cacheDir string) string {
	if cacheDir == "" {
		return ""
	}
	return filepath.Join(cacheDir, "upgrade-quarantine.json")
}

// NewQuarantine returns a Quarantine backed by path. Doesn't touch the
// filesystem until the first Add/Has.
func NewQuarantine(path string) *Quarantine { return &Quarantine{path: path} }

// Path is exposed for diagnostics.
func (q *Quarantine) Path() string { return q.path }

func (q *Quarantine) load() (quarantineFile, error) {
	qf := quarantineFile{Releases: map[string]QuarantineEntry{}}
	if q == nil || q.path == "" {
		return qf, nil
	}
	data, err := os.ReadFile(q.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return qf, nil
		}
		return qf, err
	}
	if err := json.Unmarshal(data, &qf); err != nil {
		return quarantineFile{Releases: map[string]QuarantineEntry{}}, err
	}
	if qf.Releases == nil {
		qf.Releases = map[string]QuarantineEntry{}
	}
	return qf, nil
}

func (q *Quarantine) save(qf quarantineFile) error {
	if err := os.MkdirAll(filepath.Dir(q.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(qf, "", "  ")
	if err != nil {
		return err
	}
	tmp := q.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, q.path)
}

// Has reports whether releaseID is quarantined. Reads the file each call so
// an entry the supervisor wrote during a revert is seen by the freshly
// restarted daemon. A read error fails OPEN (returns false): the watchdog is
// the backstop, and a re-bricked release just re-reverts — better than a
// corrupt quarantine file wedging all future upgrades.
func (q *Quarantine) Has(releaseID string) bool {
	if q == nil || q.path == "" || releaseID == "" {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	qf, err := q.load()
	if err != nil {
		return false
	}
	_, ok := qf.Releases[releaseID]
	return ok
}

// Add quarantines a release (read-modify-write, atomic). RevertedAt is
// stamped if unset.
func (q *Quarantine) Add(e QuarantineEntry) error {
	if q == nil || q.path == "" {
		return errors.New("quarantine: no path")
	}
	if e.ReleaseID == "" {
		return errors.New("quarantine: empty release_id")
	}
	if e.RevertedAt.IsZero() {
		e.RevertedAt = time.Now().UTC()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	qf, _ := q.load()
	qf.Releases[e.ReleaseID] = e
	return q.save(qf)
}

// List returns all quarantined entries (unordered).
func (q *Quarantine) List() ([]QuarantineEntry, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	qf, err := q.load()
	if err != nil {
		return nil, err
	}
	out := make([]QuarantineEntry, 0, len(qf.Releases))
	for _, e := range qf.Releases {
		out = append(out, e)
	}
	return out, nil
}

// Clear removes one release from quarantine. Missing is not an error.
func (q *Quarantine) Clear(releaseID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	qf, err := q.load()
	if err != nil {
		return err
	}
	delete(qf.Releases, releaseID)
	return q.save(qf)
}

// ClearAll empties the quarantine set.
func (q *Quarantine) ClearAll() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.save(quarantineFile{Releases: map[string]QuarantineEntry{}})
}
