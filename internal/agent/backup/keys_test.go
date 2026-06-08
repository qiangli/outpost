package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateIdentity_FirstRunGenerates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "age.key")
	id, rec, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	if id == nil || rec == nil {
		t.Fatal("identity + recipient should be non-nil")
	}
	// Identity file written, mode 0600.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("identity file mode %o, want 0600", st.Mode().Perm())
	}
	// Sibling recipient file written.
	pub, err := os.ReadFile(strings.TrimSuffix(path, ".key") + ".pub")
	if err != nil {
		t.Errorf("public key sidecar missing: %v", err)
	} else if !strings.HasPrefix(strings.TrimSpace(string(pub)), "age1") {
		t.Errorf("public key file should hold age1... value, got %q", string(pub))
	}
}

func TestLoadOrCreateIdentity_SecondCallReadsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "age.key")
	id1, _, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	id2, _, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if id1.String() != id2.String() {
		t.Error("second call should return the same persisted identity")
	}
}

func TestLoadOrCreateIdentity_EmptyPathRejected(t *testing.T) {
	if _, _, err := LoadOrCreateIdentity(""); err == nil {
		t.Error("empty path should error")
	}
}

func TestRecipientFingerprint(t *testing.T) {
	dir := t.TempDir()
	_, rec, err := LoadOrCreateIdentity(filepath.Join(dir, "age.key"))
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	fp := RecipientFingerprint(rec)
	if len(fp) != 16 {
		t.Errorf("fingerprint length %d, want 16", len(fp))
	}
	if RecipientFingerprint(nil) != "" {
		t.Error("nil recipient should yield empty fingerprint")
	}
}
