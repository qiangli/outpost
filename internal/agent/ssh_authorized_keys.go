package agent

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// authorizedKeyAllowed reloads the file for every authentication. This makes
// removal take effect immediately and keeps the key material out of process
// state and logs.
func authorizedKeyAllowed(name string, offered ssh.PublicKey) bool {
	if name == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		name = filepath.Join(home, ".ssh", "authorized_keys")
	}
	info, err := os.Stat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || !authorizedKeysOwnedByCurrentUser(info) {
		slog.Warn("ssh: authorized key file rejected", "reason", "missing, insecure, or not owned by daemon user")
		return false
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return false
	}
	for len(b) > 0 {
		key, _, _, rest, err := ssh.ParseAuthorizedKey(b)
		if err != nil {
			_, rest = splitAuthorizedKeyLine(b)
			slog.Warn("ssh: ignored malformed authorized key", "reason", err.Error())
			b = rest
			continue
		}
		if bytes.Equal(key.Marshal(), offered.Marshal()) {
			return true
		}
		b = rest
	}
	return false
}

func splitAuthorizedKeyLine(b []byte) ([]byte, []byte) {
	i := bytes.IndexByte(b, '\n')
	if i < 0 {
		return b, nil
	}
	return b[:i], b[i+1:]
}
