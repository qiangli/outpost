package upgrade

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// upgrade.pending.json holds the most recent envelope a "manual" host
// received but has not yet applied. The format is just the Envelope
// JSON shape directly — no wrapper, no version field, so the file is
// human-inspectable and the operator can `cat upgrade.pending.json |
// jq` if they want to see what's queued.
//
// One envelope at a time: a newer push overwrites the older one
// (latest-wins, matching how `lastReleaseID` dedup already treats
// in-flight pushes at a shorter timescale).

// writePendingEnvelope persists env at path. Atomic via write+rename
// so a partial write can't leave the operator looking at a corrupt
// half-envelope.
func writePendingEnvelope(path string, env Envelope) error {
	if path == "" {
		return errors.New("pending path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	body, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readPendingEnvelope returns the envelope persisted at path, or nil
// when no pending file exists. Decode errors are surfaced — a
// corrupt file means the operator either has to delete it or the
// next push will overwrite it.
func readPendingEnvelope(path string) (*Envelope, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// PendingPath returns the canonical path to upgrade.pending.json
// under the given cache dir. Public so main.go and the CLI's
// apply-pending command can both compute it without a magic string
// drifting between them.
func PendingPath(cacheDir string) string {
	if cacheDir == "" {
		return ""
	}
	return filepath.Join(cacheDir, "upgrade.pending.json")
}

// ReadPending is the public read variant — used by the apply-pending
// MCP tool / CLI to fetch the queued envelope before re-submitting
// it with Force=true.
func ReadPending(path string) (*Envelope, error) { return readPendingEnvelope(path) }

// LoadPending returns the queued envelope for this worker, if any.
// The worker captures the pending path from its State closure on
// every Apply; LoadPending reuses the same source-of-truth so the
// MCP / CLI surfaces don't need to compute the path themselves.
func (w *Worker) LoadPending() (*Envelope, error) {
	st := w.state()
	return readPendingEnvelope(st.PendingPath)
}
