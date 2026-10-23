// LAN sshd authorized-key management. These methods back both the
// `outpost ssh authorize ...` CLI subtree and the
// `outpost_ssh_authorize_*` MCP tools, keeping validation + filesystem
// access in one place.
//
// Keys live in the unit's authorized_keys file under the outpost config
// dir (see conf.SSHDAuthorizedKeysPath). Mutation does NOT trigger
// admincore's restart-debounce — the server re-reads the file on every
// authentication, so add/rm take effect immediately.
package admincore

import (
	"strings"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// AddSSHAuthorizedKey appends a public key to the sshd unit's
// authorized_keys file and returns its SHA256 fingerprint. Idempotent —
// re-adding a present key returns the same fingerprint.
func (s *Server) AddSSHAuthorizedKey(key string) (string, error) {
	if _, _, err := conf.ParseSSHAuthorizedKey(key); err != nil {
		return "", badRequest("invalid public key: %s", err.Error())
	}
	path, err := conf.SSHDAuthorizedKeysPath()
	if err != nil {
		return "", internalErr("authorized_keys path: %s", err.Error())
	}
	fp, err := conf.AddSSHAuthorizedKey(path, key)
	if err != nil {
		return "", internalErr("authorize add: %s", err.Error())
	}
	return fp, nil
}

// ListSSHAuthorizedKeys returns the SHA256 fingerprint of every authorized
// key (fingerprints only — the key material never leaves the host).
func (s *Server) ListSSHAuthorizedKeys() ([]string, error) {
	path, err := conf.SSHDAuthorizedKeysPath()
	if err != nil {
		return nil, internalErr("authorized_keys path: %s", err.Error())
	}
	fps, err := conf.ListSSHAuthorizedKeyFingerprints(path)
	if err != nil {
		return nil, internalErr("authorize list: %s", err.Error())
	}
	return fps, nil
}

// RemoveSSHAuthorizedKey drops the key matching fingerprint. Returns false
// (not an error) when no key matches — rm is idempotent. Trims whitespace
// so a pasted fingerprint with a trailing newline still matches.
func (s *Server) RemoveSSHAuthorizedKey(fingerprint string) (bool, error) {
	if strings.TrimSpace(fingerprint) == "" {
		return false, badRequest("fingerprint is required")
	}
	path, err := conf.SSHDAuthorizedKeysPath()
	if err != nil {
		return false, internalErr("authorized_keys path: %s", err.Error())
	}
	removed, err := conf.RemoveSSHAuthorizedKey(path, fingerprint)
	if err != nil {
		return false, internalErr("authorize rm: %s", err.Error())
	}
	return removed, nil
}
