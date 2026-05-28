//go:build linux

package sysinfo

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// memTotalBytes scrapes /proc/meminfo for "MemTotal: <kB>". Returns
// bytes (kB * 1024). Zero on any parse failure — caller omits the
// field then.
func memTotalBytes() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		// Format: "MemTotal:       16335952 kB"
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

// cpuModel returns the first "model name" line from /proc/cpuinfo.
// On Apple Silicon Linux VMs and most x86 boxes this is the marketing
// string; on bare ARM SoCs it may be missing — empty return is OK.
func cpuModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "model name") {
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			return ""
		}
		return strings.TrimSpace(line[idx+1:])
	}
	return ""
}
