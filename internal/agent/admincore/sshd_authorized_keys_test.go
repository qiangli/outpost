package admincore

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testAdminKeyLine(t *testing.T) (line, fp string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	line = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " testkey"
	return line, ssh.FingerprintSHA256(pub)
}

// The admincore verbs round-trip through the real file: add → ls → rm.
func TestSSHAuthorizedKeysAdminRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	s := &Server{}

	line, fp := testAdminKeyLine(t)
	got, err := s.AddSSHAuthorizedKey(line)
	if err != nil || got != fp {
		t.Fatalf("add = %q, %v; want %q", got, err, fp)
	}
	// Idempotent re-add.
	if got, err := s.AddSSHAuthorizedKey(line); err != nil || got != fp {
		t.Fatalf("re-add = %q, %v; want %q", got, err, fp)
	}
	ls, err := s.ListSSHAuthorizedKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 1 || ls[0] != fp {
		t.Fatalf("ls = %q, want [%q]", ls, fp)
	}
	removed, err := s.RemoveSSHAuthorizedKey(fp)
	if err != nil || !removed {
		t.Fatalf("rm = %v, %v; want true", removed, err)
	}
	ls, err = s.ListSSHAuthorizedKeys()
	if err != nil || len(ls) != 0 {
		t.Fatalf("ls after rm = %q, %v", ls, err)
	}
	// Unknown fingerprint removes nothing, without error.
	if removed, err := s.RemoveSSHAuthorizedKey(fp); err != nil || removed {
		t.Fatalf("rm missing = %v, %v; want false", removed, err)
	}
	// Garbage in is a 400, not a 500.
	if _, err := s.AddSSHAuthorizedKey("not-a-key"); err == nil {
		t.Error("adding garbage should fail")
	} else if ae := AsAPIError(err); ae == nil || ae.Status != 400 {
		t.Errorf("garbage add status = %v, want 400", ae)
	}
	if _, err := s.RemoveSSHAuthorizedKey(""); err == nil {
		t.Error("empty fingerprint should fail")
	}
}
