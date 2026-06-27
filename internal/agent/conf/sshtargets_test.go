package conf

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSSHTargetCRUD covers the round-trip: save → load → list →
// delete, and the name-validation guard.
func TestSSHTargetCRUD(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp) // for darwin's UserConfigDir fallback

	dir, err := SSHTargetsDir()
	if err != nil {
		t.Fatalf("SSHTargetsDir: %v", err)
	}
	if !strings.HasPrefix(dir, tmp) {
		t.Fatalf("SSHTargetsDir %q not under tmp %q (XDG_CONFIG_HOME ignored on this platform?)", dir, tmp)
	}

	// Save two targets.
	if err := SaveSSHTarget(SSHTarget{Name: "lab", Host: "host-b", User: "noviadmin"}); err != nil {
		t.Fatalf("save lab: %v", err)
	}
	if err := SaveSSHTarget(SSHTarget{Name: "design", Host: "host-c", User: "noviadmin", Description: "design VM"}); err != nil {
		t.Fatalf("save design: %v", err)
	}

	// Load round-trips.
	lab, err := LoadSSHTarget("lab")
	if err != nil {
		t.Fatalf("load lab: %v", err)
	}
	if lab.Host != "host-b" || lab.User != "noviadmin" {
		t.Errorf("lab round-trip mismatch: %+v", lab)
	}

	// List sorts by name.
	ts, err := ListSSHTargets()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ts) != 2 {
		t.Fatalf("len=%d, want 2: %+v", len(ts), ts)
	}
	if ts[0].Name != "design" || ts[1].Name != "lab" {
		t.Errorf("list not sorted: %+v", ts)
	}

	// Delete is idempotent.
	if err := DeleteSSHTarget("lab"); err != nil {
		t.Fatalf("delete lab: %v", err)
	}
	if err := DeleteSSHTarget("lab"); err != nil {
		t.Fatalf("delete lab (second time, should be idempotent): %v", err)
	}
	if _, err := LoadSSHTarget("lab"); err == nil {
		t.Fatal("expected LoadSSHTarget to error on missing alias")
	}
}

// TestSSHTargetNameValidation pins the charset rule. Names hit the
// filesystem directly; a permissive rule would be a path-traversal
// foothold.
func TestSSHTargetNameValidation(t *testing.T) {
	bad := []string{"", "..", "../etc/passwd", "with space", "with/slash", "with\\backslash", "with*glob"}
	for _, n := range bad {
		if err := ValidSSHTargetName(n); err == nil {
			t.Errorf("ValidSSHTargetName(%q) accepted; expected rejection", n)
		}
	}
	good := []string{"lab", "lab.1", "Lab_2", "my-home-vm", "Z99"}
	for _, n := range good {
		if err := ValidSSHTargetName(n); err != nil {
			t.Errorf("ValidSSHTargetName(%q) rejected unexpectedly: %v", n, err)
		}
	}
}

// TestResolveSSHTargetChain covers the three interesting shapes the
// resolver has to handle: a flat target (no Via), a two-hop chain,
// and a cycle that has to be rejected.
func TestResolveSSHTargetChain(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)

	mustSave := func(s SSHTarget) {
		t.Helper()
		if err := SaveSSHTarget(s); err != nil {
			t.Fatalf("save %s: %v", s.Name, err)
		}
	}

	// Flat: no Via → chain == [inner].
	mustSave(SSHTarget{Name: "flat", Host: "host-b", User: "u"})
	got, err := ResolveSSHTargetChain("flat", "")
	if err != nil {
		t.Fatalf("flat: %v", err)
	}
	if len(got) != 1 || got[0].Name != "flat" {
		t.Errorf("flat chain wrong: %+v", got)
	}

	// Two-hop: lab → via gateway. Resolver returns outer-first so the
	// first element is the cloudbox-dialed gateway and the last is the
	// requested inner target.
	mustSave(SSHTarget{Name: "gateway", Host: "gateway-host", User: "u"})
	mustSave(SSHTarget{Name: "lab", Host: "192.168.1.50", User: "u", Via: "gateway", Port: 22})
	got, err = ResolveSSHTargetChain("lab", "")
	if err != nil {
		t.Fatalf("two-hop: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("two-hop length=%d, want 2: %+v", len(got), got)
	}
	if got[0].Name != "gateway" || got[1].Name != "lab" {
		t.Errorf("two-hop order wrong: [%s, %s], want [gateway, lab]", got[0].Name, got[1].Name)
	}

	// Override at the leaf: ssh exec lab --jump otherGw should swap
	// gateway for otherGw at the innermost level.
	mustSave(SSHTarget{Name: "otherGw", Host: "other-gw", User: "u"})
	got, err = ResolveSSHTargetChain("lab", "otherGw")
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if len(got) != 2 || got[0].Name != "otherGw" {
		t.Errorf("override didn't replace via: %+v", got)
	}

	// Cycle: a → via b → via a. Must be rejected with a clear error.
	mustSave(SSHTarget{Name: "cyclea", Host: "h", User: "u", Via: "cycleb"})
	mustSave(SSHTarget{Name: "cycleb", Host: "h", User: "u", Via: "cyclea"})
	_, err = ResolveSSHTargetChain("cyclea", "")
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected cycle rejection, got: %v", err)
	}

	// Missing via target surfaces the underlying "no ssh target" error
	// rather than a generic chain error.
	mustSave(SSHTarget{Name: "danglesrc", Host: "h", User: "u", Via: "nonexistent"})
	_, err = ResolveSSHTargetChain("danglesrc", "")
	if err == nil || !strings.Contains(err.Error(), "no ssh target") {
		t.Errorf("expected missing-via error, got: %v", err)
	}
}

// TestSSHTargetSaveValidatesHost ensures Save refuses an empty host —
// otherwise ExecSSH would just fail later with a noisier error.
func TestSSHTargetSaveValidatesHost(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)
	if err := SaveSSHTarget(SSHTarget{Name: "lab", Host: ""}); err == nil {
		t.Fatal("expected SaveSSHTarget to reject empty host")
	}
}

// TestSessionCookieCRUD covers the elev-cookie cache that admincore.
// ExecSSH reads and `outpost connect` writes — round-trip both paths
// against a tempdir so XDG_CACHE_HOME isolation is intact.
func TestSessionCookieCRUD(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("HOME", tmp)

	got, err := ReadSessionCookie("nohost")
	if err != nil {
		t.Fatalf("ReadSessionCookie(missing) returned error: %v", err)
	}
	if got != "" {
		t.Fatalf("ReadSessionCookie(missing) = %q, want empty", got)
	}

	if err := WriteSessionCookie("alpha", "tok-1"); err != nil {
		t.Fatalf("WriteSessionCookie: %v", err)
	}
	got, err = ReadSessionCookie("alpha")
	if err != nil {
		t.Fatalf("ReadSessionCookie: %v", err)
	}
	if got != "tok-1" {
		t.Errorf("round-trip mismatch: got %q want tok-1", got)
	}

	// Path sanitization: hostile name shouldn't escape the sessions
	// dir. Hostile bytes become _, so this also confirms the file
	// lands inside the sessions dir.
	path, err := SessionCookiePath("../../../etc/passwd")
	if err != nil {
		t.Fatalf("SessionCookiePath(traversal): %v", err)
	}
	if !strings.HasPrefix(path, tmp) {
		t.Errorf("hostile name escaped: %q (tmp=%q)", path, tmp)
	}
	if filepath.Base(path) == "passwd" {
		t.Errorf("hostile name not sanitized: %q", path)
	}
}
