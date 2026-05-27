// Copyright (c) 2026, the outpost authors
// See LICENSE for licensing information

package shell

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// JobRecord is one row in the persistent background-job registry. The PID
// is both the kernel PID (so `outpost kill <pid>` is equivalent to plain
// `kill <pid>`) and the filename key on disk.
type JobRecord struct {
	PID       int       `json:"pid"`
	User      string    `json:"user"`
	Cmd       string    `json:"cmd"`
	StartedAt time.Time `json:"started_at"`
}

// JobRegistry persists detached background jobs the matrix shell spawned.
// One JSON file per job at <UserCacheDir>/outpost/jobs/<pid>.json.
//
// Concurrency: inserts use temp-file + rename, which is atomic on POSIX.
// Two outposts can't both be running on one host (the cmd/outpost/main.go
// pidfile claim refuses), so cross-process serialization is unnecessary.
// The external CLI only reads and deletes, never inserts.
type JobRegistry struct {
	dir string // empty = no-op (UserCacheDir not available)
}

// NewJobRegistry constructs a registry rooted at dir. Pass "" to disable
// (Record/List/Delete become no-ops); useful in tests and on hosts where
// os.UserCacheDir fails.
func NewJobRegistry(dir string) *JobRegistry { return &JobRegistry{dir: dir} }

// DefaultJobsDir is the path the default registry writes to. Caller is
// responsible for MkdirAll — the registry creates it lazily on first
// Record, so a host that never backgrounds anything has no dir at all.
func DefaultJobsDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "outpost", "jobs"), nil
}

var defaultRegistry = sync.OnceValue(func() *JobRegistry {
	dir, _ := DefaultJobsDir() // empty on error → registry no-ops
	return &JobRegistry{dir: dir}
})

// DefaultRegistry is the process-wide registry rooted at DefaultJobsDir.
// The shell.NewSession callback writes here so the external CLI's
// JobRegistry (same path) reads what was recorded.
func DefaultRegistry() *JobRegistry { return defaultRegistry() }

// Record inserts a job for the given kernel PID. The cmd is informational
// — the fork's WithBgPidCallback callback signature is (pid int) only, so
// outpost records "(detached)" until a richer callback ships.
func (r *JobRegistry) Record(pid int, cmd string) error {
	if r.dir == "" {
		return errors.New("bgjobs: no cache dir")
	}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return err
	}
	rec := JobRecord{
		PID:       pid,
		User:      currentOSUser(),
		Cmd:       cmd,
		StartedAt: time.Now().UTC(),
	}
	return writeRecord(filepath.Join(r.dir, strconv.Itoa(pid)+".json"), rec)
}

// List returns all currently-recorded jobs, sorted by PID, with dead
// records pruned in-place. A "dead" PID is one syscall.Kill(pid, 0)
// reports as ESRCH — alive-but-not-owned (EPERM) is treated as alive,
// which is conservative.
func (r *JobRegistry) List() ([]JobRecord, error) {
	if r.dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var rows []JobRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(r.dir, e.Name())
		rec, err := readRecord(path)
		if err != nil {
			continue
		}
		if !pidAlive(rec.PID) {
			_ = os.Remove(path)
			continue
		}
		rows = append(rows, rec)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].PID < rows[j].PID })
	return rows, nil
}

// Get returns one record. Returns fs.ErrNotExist if pid is not registered
// or if its file was pruned.
func (r *JobRegistry) Get(pid int) (JobRecord, error) {
	if r.dir == "" {
		return JobRecord{}, fs.ErrNotExist
	}
	return readRecord(filepath.Join(r.dir, strconv.Itoa(pid)+".json"))
}

// Delete removes the registry entry for pid. No effect on the OS-level
// process — use syscall.Kill for that.
func (r *JobRegistry) Delete(pid int) error {
	if r.dir == "" {
		return nil
	}
	err := os.Remove(filepath.Join(r.dir, strconv.Itoa(pid)+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func writeRecord(path string, rec JobRecord) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rec); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func readRecord(path string) (JobRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return JobRecord{}, err
	}
	defer f.Close()
	var rec JobRecord
	return rec, json.NewDecoder(f).Decode(&rec)
}

// pidAlive reports whether the given PID names a live process. Uses
// pidAlive lives in pidalive_unix.go / pidalive_windows.go — the
// liveness probe is the only os-specific bit in this file, so split
// it out behind build tags rather than tagging the whole module.

func currentOSUser() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}
