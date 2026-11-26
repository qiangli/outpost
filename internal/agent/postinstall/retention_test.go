package postinstall

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostInstallEvidenceRetention(t *testing.T) {
	dir := t.TempDir()
	// Simulate an interrupted publication. A retry reuses the scratch slot.
	tmp := filepath.Join(dir, ".post-install.next")
	if err := os.WriteFile(tmp, []byte("interrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		for _, name := range []string{ReportName, RebootReportName, PendingName} {
			if err := Write(dir, name, Report{Result: "PASS"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 4 {
		t.Fatalf("report generations %v %v", files, err)
	}
	if err := Write(dir, ReportName, Report{Result: strings.Repeat("x", maxReportBytes)}); err == nil {
		t.Fatal("accepted oversized evidence")
	}
	b, err := os.ReadFile(filepath.Join(dir, ReportName))
	if err != nil || !bytes.Contains(b, []byte("PASS")) {
		t.Fatal("replaced last valid evidence")
	}
}
