package sysinfo

import "testing"

// TestWMIVRAMLooksCapped pins the rule that keeps a Windows host from
// advertising a GPU it cannot measure. The observed value is the one a
// real 8 GiB RTX 3070 reports through Win32_VideoController — note it is
// NOT the 4294967295 sentinel the naive "== uint32 max" check looks for,
// which is exactly why that check was never enough.
func TestWMIVRAMLooksCapped(t *testing.T) {
	const (
		observedRTX3070 = uint64(4293918720) // 4 GiB - 1 MiB, from a real host
		uint32Max       = uint64(4294967295)
		fourGiB         = uint64(4) * 1024 * 1024 * 1024
	)
	capped := []struct {
		name  string
		bytes uint64
	}{
		{"observed 8GiB RTX 3070 reading", observedRTX3070},
		{"uint32 max sentinel", uint32Max},
		{"exactly 4GiB", fourGiB},
	}
	for _, c := range capped {
		t.Run(c.name, func(t *testing.T) {
			if !wmiVRAMLooksCapped(c.bytes) {
				t.Errorf("wmiVRAMLooksCapped(%d) = false, want true", c.bytes)
			}
		})
	}

	trusted := []struct {
		name  string
		bytes uint64
	}{
		// Below the cap window: a small card really is this small.
		{"2GiB integrated", 2 * 1024 * 1024 * 1024},
		{"3GiB", 3 * 1024 * 1024 * 1024},
		// Above the cap: the field could not have produced this, so it
		// came from a source that can count past 4 GiB.
		{"8GiB", 8 * 1024 * 1024 * 1024},
		{"24GiB", 24 * 1024 * 1024 * 1024},
		{"unknown/zero", 0},
	}
	for _, c := range trusted {
		t.Run(c.name, func(t *testing.T) {
			if wmiVRAMLooksCapped(c.bytes) {
				t.Errorf("wmiVRAMLooksCapped(%d) = true, want false", c.bytes)
			}
		})
	}
}
