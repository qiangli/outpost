// SSH-target persistence: one JSON file per friendly alias under
//
//	$XDG_CONFIG_HOME/outpost/ssh/<name>.json   (mode 0600)
//
// A target maps a local alias ("lab") to a cloudbox-paired host name
// plus optional override OS user. The new `outpost ssh ...` subtree
// (and the matching MCP tools) read/write these files; the existing
// `outpost remote` / `outpost ssh-proxy` / `outpost ssh-config`
// commands are untouched.
//
// Why a separate file per target rather than another field on
// FileConfig:
//   - FileConfig changes trigger a daemon restart (the restart-debounce
//     timer in admincore fires on any save). Friendly-alias CRUD
//     shouldn't restart anything.
//   - Mirrors the existing `outpost remote` pattern (`cmd/outpost/
//     remote.go`), which stores MCP-bearer caches the same way. One
//     reviewer, one mental model.
//   - The admin UI / MCP / CLI can all converge on the same on-disk
//     format without admincore mutex coordination.
package conf

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SSHTarget is the on-disk shape of one friendly alias.
type SSHTarget struct {
	// Name is the local alias the operator types: `outpost ssh <name>`.
	// One alias per file; the filename is `<name>.json`.
	Name string `json:"name"`

	// Host is the cloudbox-paired host name — what `outpost ssh-config`
	// would print as the `Host` stanza, and what cloudbox routes
	// `/h/<host>/ssh` to. Required.
	Host string `json:"host"`

	// User overrides the OS username the remote outpost's /auth gate
	// expects. Empty = resolve from cloudbox's /api/v1/ssh/hosts
	// at connect time (same fallback chain `outpost connect` uses).
	User string `json:"user,omitempty"`

	// Description is a freeform note for the operator's benefit
	// (printed by `outpost ssh list`). Not interpreted.
	Description string `json:"description,omitempty"`
}

// SSHTargetsDir is `<UserConfigDir>/outpost/ssh`. Created on demand.
func SSHTargetsDir() (string, error) {
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfgDir, "outpost", "ssh"), nil
}

// SSHTargetPath is the canonical on-disk path for a given alias.
func SSHTargetPath(name string) (string, error) {
	if err := ValidSSHTargetName(name); err != nil {
		return "", err
	}
	dir, err := SSHTargetsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

// KnownHostsPath is the OpenSSH-format known_hosts file the in-process
// SSH client uses for trust-on-first-use host-key pinning. Lives next
// to the per-target files so removing the outpost config dir wipes
// both at once.
func KnownHostsPath() (string, error) {
	dir, err := SSHTargetsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "known_hosts"), nil
}

// ValidSSHTargetName guards against path traversal — the name lands
// directly in a filesystem path. Same charset rule remote.go uses
// for cached MCP-bearer aliases, intentionally consistent so operators
// don't have to remember two flavors of "what's a valid alias."
//
// In addition to the charset, "." and ".." are rejected outright (they
// are valid character sequences but would resolve to filesystem
// path components — a `..` target file would be readable as the
// sessions dir's parent rather than a per-alias file). Leading "."
// is also rejected to avoid creating hidden files inadvertently.
func ValidSSHTargetName(name string) error {
	if name == "" {
		return fmt.Errorf("ssh target name is required")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("invalid ssh target name %q (reserved path component)", name)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("invalid ssh target name %q (cannot start with '.')", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("invalid ssh target name %q (allowed: letters, digits, -, _, .)", name)
		}
	}
	return nil
}

// SaveSSHTarget writes the target atomically (write to tmp + rename).
// Overwrites an existing file with the same name.
func SaveSSHTarget(t SSHTarget) error {
	if err := ValidSSHTargetName(t.Name); err != nil {
		return err
	}
	if strings.TrimSpace(t.Host) == "" {
		return fmt.Errorf("ssh target host is required")
	}
	dir, err := SSHTargetsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, t.Name+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadSSHTarget reads one target by alias. Returns a clear "no such
// target" error when the file doesn't exist — distinguishable from a
// parse failure so callers can surface a useful message.
func LoadSSHTarget(name string) (*SSHTarget, error) {
	path, err := SSHTargetPath(name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no ssh target named %q — run `outpost ssh add %s --host <paired-host>`", name, name)
		}
		return nil, err
	}
	var t SSHTarget
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// Force-set Name from the filename so a hand-edited file with a
	// mismatched "name" field still resolves consistently.
	t.Name = name
	return &t, nil
}

// DeleteSSHTarget removes the file. Idempotent — a missing file is
// reported as success so callers (CLI rm, MCP remove) don't need to
// special-case it.
func DeleteSSHTarget(name string) error {
	path, err := SSHTargetPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ListSSHTargets enumerates all on-disk targets, sorted by name.
// Files that fail to parse are silently skipped so one bad file
// doesn't break the listing — a parser would still surface them
// individually via LoadSSHTarget.
func ListSSHTargets() ([]SSHTarget, error) {
	dir, err := SSHTargetsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []SSHTarget{}, nil
		}
		return nil, err
	}
	out := make([]SSHTarget, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		if err := ValidSSHTargetName(name); err != nil {
			continue
		}
		t, err := LoadSSHTarget(name)
		if err != nil {
			continue
		}
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
