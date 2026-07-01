//go:build windows

package sysload

import (
	"sync"
	"syscall"
	"unsafe"
)

// probeLoad on Windows reads CPU utilization from GetSystemTimes
// (delta-based) and memory availability from GlobalMemoryStatusEx. There
// is no load-average concept, so load1 is -1. Both APIs are cgo-free
// kernel32 calls via syscall (matching the existing sysinfo_windows.go
// approach — no new dependency).
func probeLoad() (cpuPct, memAvailFrac, load1 float64) {
	return readCPUPct(), readMemAvailFrac(), -1
}

var (
	cpuMu       sync.Mutex
	cpuPrevIdle uint64
	cpuPrevBusy uint64
	cpuHave     bool
)

type filetime struct {
	Low  uint32
	High uint32
}

func (f filetime) u64() uint64 { return uint64(f.High)<<32 | uint64(f.Low) }

// readCPUPct calls GetSystemTimes for idle/kernel/user filetimes and
// computes utilization from the delta since the previous sample. kernel
// time already includes idle time, so busy = (kernel+user) - idle.
func readCPUPct() float64 {
	mod := syscall.NewLazyDLL("kernel32.dll")
	proc := mod.NewProc("GetSystemTimes")
	var idle, kernel, user filetime
	ret, _, _ := proc.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if ret == 0 {
		return -1
	}
	idleT := idle.u64()
	total := kernel.u64() + user.u64()
	busy := total - idleT

	cpuMu.Lock()
	defer cpuMu.Unlock()
	if !cpuHave {
		cpuPrevIdle, cpuPrevBusy, cpuHave = idleT, busy, true
		return -1
	}
	// total delta = busy delta + idle delta.
	dBusy := busy - cpuPrevBusy
	dIdle := idleT - cpuPrevIdle
	cpuPrevIdle, cpuPrevBusy = idleT, busy
	dTotal := dBusy + dIdle
	if dTotal == 0 {
		return -1
	}
	pct := float64(dBusy) / float64(dTotal) * 100
	if pct < 0 {
		return -1
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

func readMemAvailFrac() float64 {
	type memoryStatusEx struct {
		Length               uint32
		MemoryLoad           uint32
		TotalPhys            uint64
		AvailPhys            uint64
		TotalPageFile        uint64
		AvailPageFile        uint64
		TotalVirtual         uint64
		AvailVirtual         uint64
		AvailExtendedVirtual uint64
	}
	mod := syscall.NewLazyDLL("kernel32.dll")
	proc := mod.NewProc("GlobalMemoryStatusEx")
	var s memoryStatusEx
	s.Length = uint32(unsafe.Sizeof(s))
	ret, _, _ := proc.Call(uintptr(unsafe.Pointer(&s)))
	if ret == 0 || s.TotalPhys == 0 {
		return -1
	}
	return float64(s.AvailPhys) / float64(s.TotalPhys)
}
