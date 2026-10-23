package conf

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testPubKeyLine(t *testing.T, comment string) (line, fingerprint string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	if comment != "" {
		blob += " " + comment
	}
	return blob, ssh.FingerprintSHA256(pub)
}

// Add → ls → rm round-trips through the real file, including idempotent
// re-add and comment preservation.
func TestSSHAuthorizedKeysRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sshd_authorized_keys")
	line1, fp1 := testPubKeyLine(t, "laptop")
	line2, fp2 := testPubKeyLine(t, "")

	if got, err := AddSSHAuthorizedKey(path, line1); err != nil || got != fp1 {
		t.Fatalf("add1 = %q, %v; want %q", got, err, fp1)
	}
	if got, err := AddSSHAuthorizedKey(path, line2); err != nil || got != fp2 {
		t.Fatalf("add2 = %q, %v; want %q", got, err, fp2)
	}
	// Re-adding the same key is a no-op returning the same fingerprint,
	// and must not append a duplicate line.
	if got, err := AddSSHAuthorizedKey(path, line1); err != nil || got != fp1 {
		t.Fatalf("re-add = %q, %v; want %q", got, err, fp1)
	}
	ls, err := ListSSHAuthorizedKeyFingerprints(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 2 || ls[0] != fp1 || ls[1] != fp2 {
		t.Fatalf("ls = %q, want [%q %q]", ls, fp1, fp2)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "laptop") {
		t.Errorf("stored file lost the comment: %q", raw)
	}
	if n := strings.Count(strings.TrimSpace(string(raw)), "\n"); n != 1 {
		t.Errorf("stored file has %d lines, want 2 (duplicate appended)", n+1)
	}

	removed, err := RemoveSSHAuthorizedKey(path, fp1)
	if err != nil || !removed {
		t.Fatalf("rm = %v, %v; want true", removed, err)
	}
	ls, err = ListSSHAuthorizedKeyFingerprints(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 1 || ls[0] != fp2 {
		t.Fatalf("ls after rm = %q, want [%q]", ls, fp2)
	}
	removed, err = RemoveSSHAuthorizedKey(path, fp1)
	if err != nil || removed {
		t.Fatalf("rm missing = %v, %v; want false", removed, err)
	}
}

// Malformed lines never authenticate and never list, but rm preserves
// them — removal is keyed off fingerprints only.
func TestSSHAuthorizedKeysMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sshd_authorized_keys")
	line, fp := testPubKeyLine(t, "")
	if _, err := AddSSHAuthorizedKey(path, line); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("not-a-key\n# just a comment\n")
	_ = f.Close()

	ls, err := ListSSHAuthorizedKeyFingerprints(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 1 || ls[0] != fp {
		t.Fatalf("ls = %q, want [%q]", ls, fp)
	}
	if _, err := AddSSHAuthorizedKey(path, "garbage"); err == nil {
		t.Error("adding garbage should fail")
	}
	if _, err := AddSSHAuthorizedKey(path, "# comment"); err == nil {
		t.Error("adding a comment should fail")
	}
	removed, err := RemoveSSHAuthorizedKey(path, fp)
	if err != nil || !removed {
		t.Fatalf("rm = %v, %v", removed, err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "not-a-key") {
		t.Errorf("rm dropped unparseable lines: %q", raw)
	}
}

// Missing file lists as empty and removes as absent — `ls`/`rm` on a
// host that never authorized a key are not errors.
func TestSSHAuthorizedKeysMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope", "sshd_authorized_keys")
	ls, err := ListSSHAuthorizedKeyFingerprints(path)
	if err != nil || len(ls) != 0 {
		t.Fatalf("ls missing = %q, %v", ls, err)
	}
	removed, err := RemoveSSHAuthorizedKey(path, "SHA256:xxx")
	if err != nil || removed {
		t.Fatalf("rm missing = %v, %v", removed, err)
	}
}

// Default path honors XDG_CONFIG_HOME and creates the dir.
func TestSSHDAuthorizedKeysPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, err := SSHDAuthorizedKeysPath()
	if err != nil {
		t.Fatal(err)
	}
	if base := filepath.Base(p); base != SSHDAuthorizedKeysFileName {
		t.Errorf("path = %q, want .../%s", p, SSHDAuthorizedKeysFileName)
	}
	if _, err := os.Stat(filepath.Dir(p)); err != nil {
		t.Errorf("config dir not created: %v", err)
	}
}
