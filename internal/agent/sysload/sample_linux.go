//go:build linux

package sysload

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
)

// cpuPrev holds the previous /proc/stat aggregate CPU jiffie counts so a
// utilization percentage can be computed from the delta between two
// samples. Guarded by a mutex because, although Run() is the sole
// sampler, the package-level state must be safe if that ever changes.
var (
	cpuMu       sync.Mutex
	cpuPrevBusy uint64
	cpuPrevAll  uint64
	cpuHave     bool
)

// probeLoad reads Linux's /proc pseudo-files: /proc/stat for CPU
// utilization (delta-based), /proc/meminfo for available memory, and
// /proc/loadavg for the 1-minute load average. Each is independent and
// best-effort — a missing file yields -1 for that signal.
func probeLoad() (cpuPct, memAvailFrac, load1 float64) {
	return readCPUPct(), readMemAvailFrac(), readLoad1()
}

func readCPUPct() float64 {
	busy, all, ok := readProcStat()
	if !ok {
		return -1
	}
	cpuMu.Lock()
	defer cpuMu.Unlock()
	if !cpuHave {
		cpuPrevBusy, cpuPrevAll, cpuHave = busy, all, true
		return -1 // need two samples for a delta
	}
	db := busy - cpuPrevBusy
	da := all - cpuPrevAll
	cpuPrevBusy, cpuPrevAll = busy, all
	if da == 0 {
		return -1
	}
	pct := float64(db) / float64(da) * 100
	if pct < 0 {
		return -1
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

// readProcStat returns (busy, total) jiffies from the aggregate "cpu"
// line of /proc/stat. busy excludes idle + iowait.
func readProcStat() (busy, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:] // user nice system idle iowait irq softirq steal ...
		var idle uint64
		for i, fld := range fields {
			v, err := strconv.ParseUint(fld, 10, 64)
			if err != nil {
				continue
			}
			total += v
			if i == 3 || i == 4 { // idle, iowait
				idle += v
			}
		}
		return total - idle, total, true
	}
	return 0, 0, false
}

func readMemAvailFrac() float64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return -1
	}
	defer f.Close()
	var total, avail uint64
	var haveTotal, haveAvail bool
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total, haveTotal = parseMeminfoKB(line), true
		case strings.HasPrefix(line, "MemAvailable:"):
			avail, haveAvail = parseMeminfoKB(line), true
		}
		if haveTotal && haveAvail {
			break
		}
	}
	if !haveTotal || !haveAvail || total == 0 {
		return -1
	}
	return float64(avail) / float64(total)
}

func parseMeminfoKB(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func readLoad1() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return -1
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return -1
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return -1
	}
	return v
}
