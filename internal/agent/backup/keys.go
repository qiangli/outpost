package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
)

// LoadOrCreateIdentity returns the outpost-local age X25519 identity
// + matching recipient. On first call (no key file at path) a new
// identity is generated and persisted (mode 0600, parent directory
// mode 0700). The private key never leaves this host — only the
// recipient ("age1...") is published, and cloudbox / peer outposts
// see opaque ciphertext.
//
// Documented consequence: if this file is lost, every artifact
// encrypted with the matching recipient becomes unrecoverable. The
// v2 mitigation (operator-supplied passphrase escrow) is tracked in
// the umbrella plan.
//
// Empty path is an error — we never silently fall back to a
// temporary identity because that would yield artifacts no one can
// decrypt after the daemon restarts.
func LoadOrCreateIdentity(path string) (*age.X25519Identity, *age.X25519Recipient, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil, errors.New("backup: age identity path required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, fmt.Errorf("backup: resolve age path: %w", err)
	}
	if data, err := os.ReadFile(abs); err == nil {
		id, perr := age.ParseX25519Identity(strings.TrimSpace(string(data)))
		if perr != nil {
			return nil, nil, fmt.Errorf("backup: parse age identity %s: %w", abs, perr)
		}
		return id, id.Recipient(), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("backup: read age identity: %w", err)
	}

	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, nil, fmt.Errorf("backup: generate age identity: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, nil, fmt.Errorf("backup: create age dir: %w", err)
	}
	// Use O_EXCL so a concurrent generator can't trample our key;
	// the second writer falls back to LoadOrCreate's read path on
	// the next call.
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		// If the file appeared between our ReadFile + OpenFile,
		// re-read instead of failing the daemon startup.
		if errors.Is(err, os.ErrExist) {
			return LoadOrCreateIdentity(abs)
		}
		return nil, nil, fmt.Errorf("backup: create age identity file: %w", err)
	}
	if _, err := f.WriteString(id.String() + "\n"); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("backup: write age identity: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, nil, fmt.Errorf("backup: close age identity: %w", err)
	}
	// Sibling recipient file — pure operator convenience so
	// `cat age.pub` gives the value to paste into a cloudbox
	// BackupPolicy without re-deriving from the identity.
	pubPath := strings.TrimSuffix(abs, ".key") + ".pub"
	if pubPath == abs {
		pubPath = abs + ".pub"
	}
	_ = os.WriteFile(pubPath, []byte(id.Recipient().String()+"\n"), 0o600)
	return id, id.Recipient(), nil
}

// DefaultIdentityPath returns "<cacheDir>/outpost/age.key" or empty
// string if UserCacheDir is unavailable.
func DefaultIdentityPath() string {
	cache, err := os.UserCacheDir()
	if err != nil || cache == "" {
		return ""
	}
	return filepath.Join(cache, "outpost", "age.key")
}

// RecipientFingerprint returns a short stable identifier for a
// recipient public key (first 16 hex chars of sha256). Used as the
// KeyID column on BackupArtifact so a future key rotation can
// reason about which artifacts were sealed with which key without
// joining back to a policy row.
//
// Implementation note: we hash the canonical age1 string rather than
// the raw 32 bytes so the fingerprint is computable from anything
// that can render an age recipient — no need to round-trip through
// the binary curve point.
func RecipientFingerprint(r *age.X25519Recipient) string {
	if r == nil {
		return ""
	}
	return shortSHA(r.String())
}
