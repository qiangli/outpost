package admincore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// newTestServer is a minimal admincore.Server for tests that exercise
// SSH-target CRUD + ExecSSH validation paths. ConfigPath is a tempfile
// (writable, no daemon listening); Apps + Outbound are skipped because
// the SSH target paths don't touch them.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("HOME", tmp)
	s, err := New(Deps{ConfigPath: filepath.Join(tmp, "agent.json")})
	if err != nil {
		t.Fatalf("admincore.New: %v", err)
	}
	return s
}

// TestSSHTargetCRUD covers the four admincore methods that wrap conf.
// file IO: List/Upsert/Get/Delete. Validation paths are smoke-tested
// here; the full charset coverage lives in conf/sshtargets_test.go.
func TestSSHTargetCRUDAdmincore(t *testing.T) {
	s := newTestServer(t)

	// Start empty.
	ts, err := s.ListSSHTargets()
	if err != nil {
		t.Fatalf("List (empty): %v", err)
	}
	if len(ts) != 0 {
		t.Fatalf("expected empty list, got %d", len(ts))
	}

	// Upsert one.
	saved, err := s.UpsertSSHTarget(conf.SSHTarget{Name: "lab", Host: "novicortex", User: "noviadmin"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if saved.Name != "lab" || saved.Host != "novicortex" {
		t.Errorf("Upsert round-trip mismatch: %+v", saved)
	}

	// Get round-trips.
	got, err := s.GetSSHTarget("lab")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.User != "noviadmin" {
		t.Errorf("Get.User=%q, want noviadmin", got.User)
	}

	// Get unknown → 404 APIError.
	_, err = s.GetSSHTarget("nope")
	if err == nil {
		t.Fatal("expected GetSSHTarget(nope) to error")
	}
	ae := AsAPIError(err)
	if ae == nil || ae.Status != 404 {
		t.Errorf("expected 404 APIError, got %v (status=%d)", err, statusOr(ae))
	}

	// Delete is idempotent — second call returns nil.
	if err := s.DeleteSSHTarget("lab"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.DeleteSSHTarget("lab"); err != nil {
		t.Fatalf("Delete (second): %v", err)
	}
}

// TestUpsertSSHTargetValidatesName guards the path-traversal protection
// at the admincore boundary so a malformed MCP call can't write outside
// the sessions dir.
func TestUpsertSSHTargetValidatesName(t *testing.T) {
	s := newTestServer(t)
	_, err := s.UpsertSSHTarget(conf.SSHTarget{Name: "../etc/passwd", Host: "h"})
	if err == nil {
		t.Fatal("expected rejection on traversal-shaped name")
	}
	ae := AsAPIError(err)
	if ae == nil || ae.Status != 400 {
		t.Errorf("expected 400 APIError, got %v (status=%d)", err, statusOr(ae))
	}
}

// TestUpsertSSHTargetRequiresHost confirms the "host is required" rule
// — without it, ExecSSH would just fail at run time with a confusing
// "build ws url" error.
func TestUpsertSSHTargetRequiresHost(t *testing.T) {
	s := newTestServer(t)
	_, err := s.UpsertSSHTarget(conf.SSHTarget{Name: "x", Host: ""})
	if err == nil {
		t.Fatal("expected rejection on empty host")
	}
	if !strings.Contains(err.Error(), "host is required") {
		t.Errorf("error should explain the required-field, got: %v", err)
	}
}

// TestExecSSHRejectsMissingTarget covers the "target not found" branch
// before any cloudbox dial happens.
func TestExecSSHRejectsMissingTarget(t *testing.T) {
	s := newTestServer(t)
	_, err := s.ExecSSH(context.Background(), ExecSSHParams{Name: "nope", Command: "true"})
	if err == nil {
		t.Fatal("expected error on missing target")
	}
	ae := AsAPIError(err)
	if ae == nil || ae.Status != 404 {
		t.Errorf("expected 404 APIError, got %v (status=%d)", err, statusOr(ae))
	}
}

// TestExecSSHRejectsEmptyUser covers the user-not-set branch which is
// the most likely real-world misconfiguration (cloudbox didn't report
// an os_user at add time, and the operator didn't pass --user).
func TestExecSSHRejectsEmptyUser(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.UpsertSSHTarget(conf.SSHTarget{Name: "x", Host: "h"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	_, err := s.ExecSSH(context.Background(), ExecSSHParams{Name: "x", Command: "true"})
	if err == nil {
		t.Fatal("expected error on empty user")
	}
	if !strings.Contains(err.Error(), "no user set") {
		t.Errorf("error should mention the missing user, got: %v", err)
	}
}

// statusOr is a tiny helper so the test prints a useful int when the
// error wasn't an APIError at all (vs. nil-deref panic).
func statusOr(ae *APIError) int {
	if ae == nil {
		return 0
	}
	return ae.Status
}
