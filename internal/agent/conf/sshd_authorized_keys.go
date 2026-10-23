// LAN sshd authorized-keys store: the key file backing public-key
// authentication on the `outpost.sshd` service unit.
//
// The file lives under the outpost config dir (OpenSSH `authorized_keys`
// format, one key per line), NOT in ~/.ssh: the unit must work on hosts
// where the daemon account never had an OpenSSH setup, and the operator
// manages it through `outpost ssh authorize ...` (CLI) / the matching MCP
// tools rather than by hand-editing dotfiles.
//
// The SSH server itself reads this file on every authentication
// (sshserver.AuthorizedKeyAllowed), so add/rm take effect immediately
// with no daemon restart. Keys authenticate the daemon OS account only —
// enforced by the server's PublicKeyCallback, not here.
package conf

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// SSHDAuthorizedKeysFileName is the file name inside the outpost config
// dir holding the LAN sshd unit's authorized keys.
const SSHDAuthorizedKeysFileName = "sshd_authorized_keys"

// SSHDAuthorizedKeysPath returns the default authorized_keys file for the
// `outpost.sshd` unit: `<config-dir>/sshd_authorized_keys`. The directory
// is created on demand; the file itself is created on first add.
func SSHDAuthorizedKeysPath() (string, error) {
	dir, err := DefaultConfigDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("sshd authorized_keys dir: %w", err)
	}
	return filepath.Join(dir, SSHDAuthorizedKeysFileName), nil
}

// ParseSSHAuthorizedKey parses one OpenSSH public-key line (as produced by
// `ssh-keygen`: `ssh-ed25519 AAAA... comment`) and returns the key plus
// its SHA256 fingerprint. Options-prefixed lines (e.g.
// `command="..." ssh-ed25519 ...`) are rejected: the server parses with
// ssh.ParseAuthorizedKey too, so such a line could never authenticate —
// rejecting at add time beats storing a silently dead key.
func ParseSSHAuthorizedKey(line string) (ssh.PublicKey, string, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil, "", fmt.Errorf("empty or comment line")
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, "", fmt.Errorf("parse public key: %w", err)
	}
	return key, ssh.FingerprintSHA256(key), nil
}

// ListSSHAuthorizedKeyFingerprints returns the SHA256 fingerprint of every
// valid key in the file. Missing file = no keys (not an error). Malformed
// lines are skipped — the server ignores them the same way, and `ls` shows
// fingerprints only.
func ListSSHAuthorizedKeyFingerprints(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	out := []string{}
	for len(b) > 0 {
		var line []byte
		line, b = splitKeyLine(b)
		if k, _, _, _, perr := ssh.ParseAuthorizedKey(line); perr == nil {
			out = append(out, ssh.FingerprintSHA256(k))
		}
	}
	return out, nil
}

// AddSSHAuthorizedKey appends a public key to the file, creating it (0600)
// when missing. Adding a key whose fingerprint is already present is a
// no-op returning that fingerprint — `authorize add` is idempotent. The
// stored line is the caller's line verbatim (trimmed), so comments survive
// the round-trip.
func AddSSHAuthorizedKey(path, line string) (string, error) {
	key, fp, err := ParseSSHAuthorizedKey(line)
	if err != nil {
		return "", err
	}
	if existing, rerr := os.ReadFile(path); rerr == nil {
		rest := existing
		for len(rest) > 0 {
			var l []byte
			l, rest = splitKeyLine(rest)
			if k, _, _, _, perr := ssh.ParseAuthorizedKey(l); perr == nil && bytes.Equal(k.Marshal(), key.Marshal()) {
				return fp, nil
			}
		}
	} else if !os.IsNotExist(rerr) {
		return "", rerr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("sshd authorized_keys dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(strings.TrimSpace(line) + "\n"); err != nil {
		return "", err
	}
	// Harden permissions for files that predate us (the server refuses
	// group/other-writable key files on Unix).
	_ = os.Chmod(path, 0o600)
	return fp, nil
}

// RemoveSSHAuthorizedKey drops every line whose key matches fingerprint.
// Returns true when at least one line was removed. Unparseable lines are
// preserved — removal is keyed off fingerprints, and deleting a line
// neither OpenSSH nor our server would accept is not ours to do.
func RemoveSSHAuthorizedKey(path, fingerprint string) (bool, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return false, fmt.Errorf("fingerprint is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var keep []string
	removed := false
	for len(b) > 0 {
		var raw []byte
		raw, b = splitKeyLine(b)
		line := strings.TrimRight(string(raw), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if k, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(line)); perr == nil {
			if ssh.FingerprintSHA256(k) == fingerprint {
				removed = true
				continue
			}
		}
		keep = append(keep, line)
	}
	if !removed {
		return false, nil
	}
	out := ""
	if len(keep) > 0 {
		out = strings.Join(keep, "\n") + "\n"
	}
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return false, err
	}
	return true, nil
}

func splitKeyLine(b []byte) ([]byte, []byte) {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return b[:i], b[i+1:]
	}
	return b, nil
}
