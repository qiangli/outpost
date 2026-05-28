//go:build darwin

package sysinfo

import (
	"os/exec"
	"strconv"
	"strings"
)

// memTotalBytes reads `sysctl -n hw.memsize` which the kernel
// returns as bytes (uint64). Avoids CGo by shelling out — sysctl
// is in /usr/sbin which is always on the default PATH.
func memTotalBytes() uint64 {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// cpuModel returns the marketing brand string Apple Silicon /
// Intel both ship via sysctl. Trimmed of the trailing newline.
// "" when sysctl is unavailable or returns nothing useful.
func cpuModel() string {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "machdep.cpu.brand_string").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
