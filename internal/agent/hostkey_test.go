package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadOrCreateHostKeyPersists asserts the first call generates the
// key and the second call returns the same public bytes — re-pairing
// must NOT regenerate, or every paired client sees a host-key-changed
// warning.
func TestLoadOrCreateHostKeyPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ssh_host_ed25519")

	first, err := loadOrCreateHostKeyAt(path)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if first == nil {
		t.Fatal("first call returned nil signer")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("host key file mode = %#o, want 0600", mode)
	}

	second, err := loadOrCreateHostKeyAt(path)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !bytes.Equal(first.PublicKey().Marshal(), second.PublicKey().Marshal()) {
		t.Error("second call returned a different public key — key was regenerated, which would break clients' known_hosts entries")
	}
}

func TestLoadOrCreateHostKeyRejectsCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ssh_host_ed25519")
	if err := os.WriteFile(path, []byte("not a real ssh private key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateHostKeyAt(path); err == nil {
		t.Fatal("expected parse error on corrupt key file, got nil")
	}
}
