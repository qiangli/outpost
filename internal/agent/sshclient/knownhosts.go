// Trust-on-first-use host-key pinning for the in-process SSH client.
//
// The remote outpost presents a persistent ed25519 host key from
// `internal/agent/hostkey.go`. The first time we connect to a given
// (host alias, key) pair, we pin it; subsequent connects must match.
// A mismatch is a hard failure surfaced as
// REMOTE HOST IDENTIFICATION HAS CHANGED so the operator notices.
//
// File format (one entry per line, OpenSSH-known_hosts-compatible
// for the simple case):
//
//	<alias> <key-type> <base64-marshaled-public-key>
//
// We don't use `golang.org/x/crypto/ssh/knownhosts` because that
// package's checker calls SplitHostPort on the dynamic `remote
// net.Addr` it receives from the SSH layer — and our underlying
// transport is a websocket-wrapped net.Conn whose RemoteAddr
// returns a synthetic non-parseable string. The full known_hosts
// matcher buys us no benefit for our trust model (alias-only, no
// IP/DNS pinning), so a focused 50-LOC implementation is the right
// shape.
package sshclient

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// KnownHostsCallbackTOFU returns an ssh.HostKeyCallback that:
//   - on first contact for `hostAlias`, pins the presented key (TOFU);
//   - on subsequent contact, verifies key bytes match the pinned entry;
//   - on mismatch, fails hard with REMOTE HOST IDENTIFICATION HAS CHANGED.
//
// `path` is created with mode 0600 on first pin. The parent directory
// is created if missing.
//
// `hostAlias` is the alias used in entries — typically
// `outpost-<host>` for consistency with `outpost ssh-config`.
func KnownHostsCallbackTOFU(path, hostAlias string) (ssh.HostKeyCallback, error) {
	if hostAlias == "" {
		return nil, errors.New("sshclient: empty host alias")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir known_hosts parent: %w", err)
	}
	var mu sync.Mutex
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		mu.Lock()
		defer mu.Unlock()
		pinned, ok, err := lookupKnownHost(path, hostAlias)
		if err != nil {
			return fmt.Errorf("read known_hosts %s: %w", path, err)
		}
		presented := key.Marshal()
		if ok {
			if bytes.Equal(pinned, presented) {
				return nil
			}
			return fmt.Errorf(
				"REMOTE HOST IDENTIFICATION HAS CHANGED for %q: "+
					"presented key %s does not match pinned entry — "+
					"remove the line for %s from %s if this is expected",
				hostAlias, ssh.FingerprintSHA256(key), hostAlias, path)
		}
		// First contact — TOFU pin.
		if err := appendKnownHost(path, hostAlias, key); err != nil {
			return fmt.Errorf("pin host key for %q: %w", hostAlias, err)
		}
		return nil
	}, nil
}

// lookupKnownHost reads `path` and returns the marshaled key bytes of
// the first entry whose alias matches. Returns (_, false, nil) when no
// entry exists for that alias.
func lookupKnownHost(path, hostAlias string) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if fields[0] != hostAlias {
			continue
		}
		// fields[1] = key type (e.g. ssh-ed25519), fields[2] = base64-
		// encoded marshaled wire form. The marshaled bytes already
		// embed the key type so we don't need to compare type
		// separately — bytes.Equal in the caller is sufficient.
		raw, err := base64.StdEncoding.DecodeString(fields[2])
		if err != nil {
			return nil, false, fmt.Errorf("decode pinned key for %s: %w", hostAlias, err)
		}
		return raw, true, nil
	}
	if err := sc.Err(); err != nil {
		return nil, false, err
	}
	return nil, false, nil
}

// appendKnownHost writes a fresh entry. We touch the file with mode
// 0600 first if missing so a hostile umask doesn't widen the bits.
func appendKnownHost(path, hostAlias string, key ssh.PublicKey) error {
	line := fmt.Sprintf("%s %s %s\n",
		hostAlias,
		key.Type(),
		base64.StdEncoding.EncodeToString(key.Marshal()),
	)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return err
	}
	return nil
}

// HostAliasForHost is the canonical "outpost-<host>" alias used both
// here and in `outpost ssh-config`'s emitted ~/.ssh/config stanzas.
// Centralized so the two stay in sync.
func HostAliasForHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	return "outpost-" + host
}
