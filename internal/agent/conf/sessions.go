// Per-host elevation-cookie cache.
//
// `outpost connect` writes the matrix_elev cookie for each host under
//
//	<UserCacheDir>/outpost/sessions/<host>.cookie  (mode 0600)
//
// so subsequent `outpost ssh` / `outpost ssh-proxy` / agentic-tool
// invocations can ride on it without re-prompting. The helpers live
// in `conf` rather than `cmd/outpost` so admincore can read the same
// cache when serving MCP-driven SSH execs.
package conf

import (
	"os"
	"path/filepath"
	"strings"
)

// SessionCookiePath returns the canonical on-disk path for the
// matrix_elev cookie for the given paired host. The directory is
// created with mode 0700 on first call.
//
// The filename is sanitized — cloudbox accepts arbitrary host names
// but we restrict the on-disk byte sequence to letters/digits/-_.
// so a hostile name can't traverse out of the sessions directory.
// Anything else is replaced with `_`.
func SessionCookiePath(host string) (string, error) {
	base, err := DefaultCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_' || r == '.':
			return r
		default:
			return '_'
		}
	}, host)
	return filepath.Join(dir, safe+".cookie"), nil
}

// WriteSessionCookie persists the cookie value to disk atomically
// (write to tmp + rename). Mode 0600 — same OS user only.
func WriteSessionCookie(host, cookie string) error {
	path, err := SessionCookiePath(host)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(cookie); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadSessionCookie returns the cached cookie for the host, or an empty
// string + nil error when no cookie has been cached yet (the
// "elevation required" state). A read error other than NotExist is
// propagated so callers can distinguish IO problems from "no entry."
func ReadSessionCookie(host string) (string, error) {
	path, err := SessionCookiePath(host)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
