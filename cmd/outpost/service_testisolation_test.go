//go:build outpost_testisolated

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsolatedServiceAudit(t *testing.T) {
	if os.Getenv("OUTPOST_AUDIT_CHILD") == "1" {
		serviceCommand("launchctl", "bootout", "gui/501/"+launchdLabel)
		t.Fatal("isolated runner allowed host access")
	}
	audit := filepath.Join(t.TempDir(), "audit")
	cmd := exec.Command(os.Args[0], "-test.run=^TestIsolatedServiceAudit$")
	cmd.Env = append(os.Environ(), "OUTPOST_AUDIT_CHILD=1",
		"OUTPOST_TEST_SERVICE_LABEL=audit-fixture", "OUTPOST_TEST_SERVICE_AUDIT="+audit)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("service attempt succeeded: %s", out)
	}
	b, err := os.ReadFile(audit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "gui/501/io.dhnt.test.audit-fixture") {
		t.Fatalf("missing per-test label in service audit: %s", b)
	}
}
