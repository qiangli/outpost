//go:build windows

package sysinfo

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

// memTotalBytes calls GlobalMemoryStatusEx via syscall. The kernel32
// API returns physical memory in bytes (uint64), no parsing dance
// required. Returns 0 on syscall failure.
func memTotalBytes() uint64 {
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
	if ret == 0 {
		return 0
	}
	return s.TotalPhys
}

// cpuModel shells out to PowerShell because there's no stable
// Windows-stdlib path that returns the marketing name. Wmic is being
// deprecated; Get-CimInstance survives across Win11 + Server 2022.
// Slower than reading a sysctl but only fires once per /apps poll.
func cpuModel() string {
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		"(Get-CimInstance Win32_Processor | Select-Object -First 1).Name").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// diskUsageBytes calls GetDiskFreeSpaceExW. path is the absolute
// path to the outpost data dir; Windows resolves it to the
// containing volume. Returns (0, 0) on syscall error.
func diskUsageBytes(path string) (total, free uint64) {
	if path == "" {
		return 0, 0
	}
	mod := syscall.NewLazyDLL("kernel32.dll")
	proc := mod.NewProc("GetDiskFreeSpaceExW")
	pathW, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0
	}
	var freeAvail, totalBytes, totalFree uint64
	ret, _, _ := proc.Call(
		uintptr(unsafe.Pointer(pathW)),
		uintptr(unsafe.Pointer(&freeAvail)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if ret == 0 {
		return 0, 0
	}
	return totalBytes, totalFree
}

// hostname uses os.Hostname which is stdlib-portable across
// Windows. No need for the platform-specific wrappers.
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// gpuInfo shells out to PowerShell's Get-CimInstance Win32_VideoController
// for adapter enumeration. AdapterRAM is the legacy field for VRAM
// in bytes (capped at 4 GiB by Win32 — a known issue for big
// modern cards). Newer Win11 builds expose AdapterRAM as uint64
// but the cap is still there in the WDDM layer; consumers should
// treat 4294967295 as "unknown" rather than literal.
//
// We export the raw value anyway — Phase 1 routing can sanity-check
// "4 GiB exactly" + Kind=nvidia and look at the model name as a
// fallback (e.g. "RTX 4090" → 24 GB known).
func gpuInfo() []GPU {
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		"Get-CimInstance Win32_VideoController | "+
			"Select-Object Name, AdapterRAM, AdapterCompatibility | "+
			"ConvertTo-Json -Compress").Output()
	if err != nil {
		return nil
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil
	}
	// Get-CimInstance returns a single object directly (not an
	// array) when there's exactly one controller. Normalize by
	// wrapping a non-array payload.
	if !strings.HasPrefix(raw, "[") {
		raw = "[" + raw + "]"
	}
	var rows []struct {
		Name                 string `json:"Name"`
		AdapterRAM           uint64 `json:"AdapterRAM"`
		AdapterCompatibility string `json:"AdapterCompatibility"`
	}
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil
	}
	gpus := make([]GPU, 0, len(rows))
	for _, r := range rows {
		vendor := strings.ToLower(r.AdapterCompatibility)
		kind := ""
		switch {
		case strings.Contains(vendor, "nvidia"):
			kind = "nvidia"
		case strings.Contains(vendor, "amd") || strings.Contains(vendor, "ati"):
			kind = "amd"
		case strings.Contains(vendor, "intel"):
			kind = "intel"
		}
		gpus = append(gpus, GPU{
			Kind:           kind,
			Model:          strings.TrimSpace(r.Name),
			Count:          1,
			VRAMTotalBytes: r.AdapterRAM,
		})
	}
	return gpus
}
