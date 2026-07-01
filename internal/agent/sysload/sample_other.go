//go:build !darwin && !linux && !windows

package sysload

// probeLoad is the stub for platforms outpost doesn't ship to (freebsd,
// openbsd, etc.). All signals are unknown (-1); the profiler then treats
// the host as never-busy and applies the full warm budget. Real support
// lives in the per-platform files behind build tags.
func probeLoad() (cpuPct, memAvailFrac, load1 float64) {
	return -1, -1, -1
}
