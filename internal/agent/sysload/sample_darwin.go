//go:build darwin

package sysload

import (
	"os/exec"
	"strconv"
	"strings"
)

// probeLoad on macOS reads the 1-minute load average via
// `sysctl -n vm.loadavg` and available memory via `vm_stat`. There is no
// cgo-free way to read a precise system-wide CPU percentage without a
// slow `top -l` sample, so cpuPct is returned as -1 (unknown); the
// profiler falls back to a load-average estimate for the CPU signal.
// Both binaries are part of the base OS and always on the default PATH.
func probeLoad() (cpuPct, memAvailFrac, load1 float64) {
	return -1, readMemAvailFrac(), readLoad1()
}

// readLoad1 parses `sysctl -n vm.loadavg`, whose output is
// "{ 1.23 1.45 1.67 }". Returns the first (1-minute) figure, or -1.
func readLoad1() float64 {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return -1
	}
	s := strings.TrimSpace(string(out))
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	fields := strings.Fields(s)
	if len(fields) < 1 {
		return -1
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return -1
	}
	return v
}

// readMemAvailFrac derives available/total memory from `vm_stat` (free +
// inactive + speculative pages × page size) over `sysctl hw.memsize`.
// Inactive pages are reclaimable, so counting them as "available"
// matches how the OS treats memory pressure. Returns -1 on any failure.
func readMemAvailFrac() float64 {
	total := hwMemsize()
	if total == 0 {
		return -1
	}
	out, err := exec.Command("/usr/bin/vm_stat").Output()
	if err != nil {
		return -1
	}
	pageSize := uint64(4096)
	var freePages, inactivePages, specPages uint64
	for _, line := range strings.Split(string(out), "\n") {
		lower := strings.ToLower(line)
		switch {
		case strings.Contains(lower, "page size of"):
			// "Mach Virtual Memory Statistics: (page size of 16384 bytes)"
			for _, f := range strings.Fields(line) {
				if n, err := strconv.ParseUint(f, 10, 64); err == nil && n >= 4096 {
					pageSize = n
					break
				}
			}
		case strings.HasPrefix(lower, "pages free:"):
			freePages = parseVMStatCount(line)
		case strings.HasPrefix(lower, "pages inactive:"):
			inactivePages = parseVMStatCount(line)
		case strings.HasPrefix(lower, "pages speculative:"):
			specPages = parseVMStatCount(line)
		}
	}
	avail := (freePages + inactivePages + specPages) * pageSize
	if avail == 0 {
		return -1
	}
	frac := float64(avail) / float64(total)
	if frac > 1 {
		frac = 1
	}
	return frac
}

// parseVMStatCount extracts the trailing page count from a vm_stat line
// like "Pages free:                           123456." (dot-terminated).
func parseVMStatCount(line string) uint64 {
	idx := strings.LastIndex(line, ":")
	if idx < 0 {
		return 0
	}
	s := strings.TrimSpace(line[idx+1:])
	s = strings.TrimSuffix(s, ".")
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func hwMemsize() uint64 {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}
