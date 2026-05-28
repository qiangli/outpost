//go:build darwin

package sysinfo

import "testing"

func TestParseVRAMString(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"8 GB", 8 * 1024 * 1024 * 1024},
		{"1536 MB", 1536 * 1024 * 1024},
		{"8192 MB", 8192 * 1024 * 1024},
		{"512 KB", 512 * 1024},
		// Garbage / unset / unknown units → 0 so caller can decide.
		{"", 0},
		{"unknown", 0},
		{"Apple_Default", 0},
		{"123 TB", 0}, // unsupported unit
	}
	for _, c := range cases {
		got := parseVRAMString(c.in)
		if got != c.want {
			t.Errorf("parseVRAMString(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("a", "b"); got != "a" {
		t.Errorf("a wins: got %q", got)
	}
	if got := firstNonEmpty("", "b"); got != "b" {
		t.Errorf("empty falls through: got %q", got)
	}
	if got := firstNonEmpty("  ", "b"); got != "b" {
		t.Errorf("whitespace falls through: got %q", got)
	}
}

// TestGpuInfo_Live exercises the actual system_profiler call. Skipped
// in CI / cross-compile (build tag covers that), so the only place
// this fires is darwin developers running `go test`. The assertion
// is loose — we accept "no GPUs reported" (locked-down macOS, CI
// runner with display privacy) — but a non-nil result must be
// well-formed.
func TestGpuInfo_Live(t *testing.T) {
	gs := gpuInfo()
	for _, g := range gs {
		if g.Model == "" {
			t.Errorf("GPU with empty model: %+v", g)
		}
		if g.Count <= 0 {
			t.Errorf("GPU with non-positive count: %+v", g)
		}
		// On Apple Silicon, VRAM should be set via the unified-
		// memory fallback. We can't assert "always apple-silicon"
		// because Intel + dGPU Macs also exist.
		if g.Kind == "apple-silicon" {
			if !g.UnifiedMemory {
				t.Errorf("apple-silicon without unified_memory: %+v", g)
			}
			if g.VRAMTotalBytes == 0 {
				t.Errorf("apple-silicon with zero VRAM (expected unified-mem fallback): %+v", g)
			}
		}
	}
}
