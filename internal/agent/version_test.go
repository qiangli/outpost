package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildInfoShort(t *testing.T) {
	cases := []struct {
		in   BuildInfo
		want string
	}{
		{BuildInfo{}, "unknown"},
		{BuildInfo{Commit: "06d8d8912345abc"}, "06d8d89"},
		{BuildInfo{Commit: "06d8d8912345abc", Dirty: true}, "06d8d89-dirty"},
		{BuildInfo{Commit: "abc"}, "abc"},
	}
	for _, tc := range cases {
		if got := tc.in.Short(); got != tc.want {
			t.Errorf("Short(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestReadBuildInfoGoVersion(t *testing.T) {
	// Under `go test` the runtime always reports a Go version. Commit/VCS
	// fields may legitimately be empty (tests run without VCS settings
	// stamped), so we only assert the always-populated field.
	if got := ReadBuildInfo(); got.GoVersion == "" {
		t.Error("ReadBuildInfo().GoVersion is empty")
	}
}

func TestInitBootCountMonotonicPersisted(t *testing.T) {
	t.Cleanup(func() { bootCount = 0 }) // package var — don't leak to other tests
	dir := t.TempDir()

	InitBootCount(dir)
	if bootCount != 1 {
		t.Fatalf("first InitBootCount → bootCount=%d, want 1", bootCount)
	}
	if got := ReadBuildInfo().BootCount; got != 1 {
		t.Fatalf("ReadBuildInfo().BootCount=%d, want 1", got)
	}

	InitBootCount(dir) // a "restart" against the same persisted counter
	if bootCount != 2 {
		t.Fatalf("second InitBootCount → bootCount=%d, want 2 (must persist)", bootCount)
	}
	data, err := os.ReadFile(filepath.Join(dir, "boot_count"))
	if err != nil || strings.TrimSpace(string(data)) != "2" {
		t.Fatalf("persisted boot_count = %q, %v; want \"2\"", string(data), err)
	}

	InitBootCount("") // empty dir is a no-op, not a crash
	if bootCount != 2 {
		t.Fatalf("InitBootCount(\"\") changed bootCount to %d", bootCount)
	}
}

func TestHealthyProbeReporting(t *testing.T) {
	t.Cleanup(func() { HealthyProbe = nil })

	// No probe wired → assume healthy.
	HealthyProbe = nil
	if !ReadBuildInfo().Healthy {
		t.Error("with no HealthyProbe, Healthy should default true")
	}

	// Probe reporting unhealthy (e.g. a pending unconfirmed upgrade).
	HealthyProbe = func() bool { return false }
	if ReadBuildInfo().Healthy {
		t.Error("HealthyProbe=false should make Healthy false")
	}

	HealthyProbe = func() bool { return true }
	if !ReadBuildInfo().Healthy {
		t.Error("HealthyProbe=true should make Healthy true")
	}
}
