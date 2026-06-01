// Trust-on-first-use host-key pinning for the in-process SSH client.
//
// The remote outpost presents a persistent ed25519 host key from
// `internal/agent/hostkey.go`. The first time we connect to a given
// (host alias, key) pair, we pin it; subsequent connects must match.
// A mismatch is a hard failure surfaced as
// REMOTE HOST IDENTIFICATION HAS CHANGED so the operator notices.
//
// The pinning alias is `outpost-<host>` — identical to what
// `outpost ssh-config`'s emitted stanzas use (line 323 of
// `cmd/outpost/ssh.go`). Operators who already trust a host via the
// system-ssh path don't have to re-trust via the in-process path.
package sshclient

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// KnownHostsCallbackTOFU returns an ssh.HostKeyCallback that:
//   - validates the presented key against `path` (OpenSSH known_hosts
//     format) using `golang.org/x/crypto/ssh/knownhosts`;
//   - on "no entry for this alias", pins the key as a new entry
//     (trust-on-first-use);
//   - on "entry exists but key mismatches", fails hard.
//
// `path` is created with mode 0600 on first pin. The directory is
// expected to exist (callers should mkdir the SSH-targets dir first;
// SaveSSHTarget does this for us in the normal path).
//
// `hostAlias` is the alias used in known_hosts entries — typically
// `outpost-<host>` for consistency with `outpost ssh-config`.
func KnownHostsCallbackTOFU(path, hostAlias string) (ssh.HostKeyCallback, error) {
	if hostAlias == "" {
		return nil, errors.New("sshclient: empty host alias")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir known_hosts parent: %w", err)
	}
	// knownhosts.New requires the file to exist. Touch it so a fresh
	// install can pin the first connection without a preceding empty-
	// file create step.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create %s: %w", path, err)
		}
		_ = f.Close()
	}

	// Pinning + verifying need to be serialized to avoid the classic
	// double-pin race: two concurrent connects, both miss the entry,
	// both append. Cheap mutex; concurrency for SSH dials inside one
	// outpost process is low.
	var mu sync.Mutex
	return func(_ string, remote net.Addr, key ssh.PublicKey) error {
		mu.Lock()
		defer mu.Unlock()

		// Re-read on every call so a freshly-pinned entry from a
		// previous connection in the same process picks up the new
		// key. knownhosts.New caches the file at load time.
		verify, err := knownhosts.New(path)
		if err != nil {
			return fmt.Errorf("re-load known_hosts %s: %w", path, err)
		}
		// We always identify the peer by hostAlias, ignoring the
		// dynamic hostname the SSH layer received (which would be the
		// WSS URL — not the right thing to pin). knownhosts requires a
		// `host:port` shape for its address argument; we synthesize
		// :22 (the openssh default) so the matcher round-trips with
		// the entries we wrote.
		err = verify(hostAlias+":22", remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) {
			if len(keyErr.Want) > 0 {
				// Mismatch: we have a different key on file for this
				// alias. Refuse loudly so the operator notices.
				return fmt.Errorf(
					"REMOTE HOST IDENTIFICATION HAS CHANGED for %q: "+
						"presented key %s does not match pinned entry — "+
						"remove the line for %s from %s if this is expected",
					hostAlias, ssh.FingerprintSHA256(key), hostAlias, path)
			}
			// No entry at all: TOFU-pin it.
			if err := appendKnownHost(path, hostAlias, key); err != nil {
				return fmt.Errorf("pin host key for %q: %w", hostAlias, err)
			}
			return nil
		}
		return err
	}, nil
}

// appendKnownHost appends a fresh OpenSSH-format known_hosts entry for
// the given alias + key. Uses `knownhosts.Line` so the entry round-trips
// through the same parser. The synthesized `:22` matches the address
// shape KnownHostsCallbackTOFU passes to verify, so subsequent
// lookups land on the entry.
func appendKnownHost(path, hostAlias string, key ssh.PublicKey) error {
	line := knownhosts.Line([]string{hostAlias + ":22"}, key)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	// knownhosts.Line returns a line without a trailing newline.
	if _, err := f.WriteString(line + "\n"); err != nil {
		return err
	}
	return nil
}

// hostAliasForHost is the canonical "outpost-<host>" alias used both
// here and in `outpost ssh-config`'s emitted ~/.ssh/config stanzas.
// Centralized so the two stay in sync.
func HostAliasForHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	return "outpost-" + host
}
