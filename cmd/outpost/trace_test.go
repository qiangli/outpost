package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEmitStartupTrace_WritesLine confirms that calling
// emitStartupTrace from a CLI invocation appends one timestamped line
// containing the pid/ppid/argv/cwd/PATH context. This is the durable
// post-mortem trail that lets us diagnose "outpost was killed in 1 ms
// with no log output" reports.
func TestEmitStartupTrace_WritesLine(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)

	emitStartupTrace()

	path := filepath.Join(tmp, "outpost", "cli-trace.log")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace log: %v", err)
	}
	got := string(b)
	// Spot-check the fields we rely on for diagnosis. We don't pin
	// the exact format here so the diagnostic line can evolve.
	for _, want := range []string{"pid=", "ppid=", "argv0=", "path="} {
		if !strings.Contains(got, want) {
			t.Errorf("trace line missing %q field: %q", want, got)
		}
	}
}

// TestEmitStartupTrace_OptOut confirms OUTPOST_TRACE=0 disables the
// write — important for hermetic CI runs that don't want any
// filesystem side effects from binary startup.
func TestEmitStartupTrace_OptOut(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("OUTPOST_TRACE", "0")

	emitStartupTrace()

	if _, err := os.Stat(filepath.Join(tmp, "outpost", "cli-trace.log")); err == nil {
		t.Fatal("trace log was created despite OUTPOST_TRACE=0")
	}
}

// TestEmitStartupTrace_SwallowsErrors confirms that a non-writable
// cache dir is silently tolerated — the CLI must never abort because
// the diagnostic trail couldn't be appended.
func TestEmitStartupTrace_SwallowsErrors(t *testing.T) {
	// Point XDG_CACHE_HOME at a non-directory (regular file) so
	// MkdirAll fails. The function must not panic or return.
	tmp := t.TempDir()
	bogus := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(bogus, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", bogus)

	// If this panics or hangs the test fails.
	emitStartupTrace()
}
