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

// gpuInfo enumerates the host's adapters, preferring the vendor tool
// over WMI.
//
// nvidia-smi FIRST, and not merely for symmetry with Linux: WMI's
// Win32_VideoController.AdapterRAM is a legacy uint32 field capped at
// 4 GiB, so it physically cannot describe a modern card. An 8 GiB
// RTX 3070 reports 4293918720 there; nvidia-smi reports the true 8192
// MiB. Since VRAMTotalBytes feeds VRAM-headroom routing and the
// dhnt.io/vram node capacity, taking WMI's word would understate every
// large NVIDIA card by 2x or more.
//
// WMI remains the fallback for non-NVIDIA adapters (Intel iGPUs, AMD),
// where it is the only enumeration available — but a reading that is
// indistinguishable from the 4 GiB cap is reported as unknown (0)
// rather than passed off as a measurement. See wmiVRAMLooksCapped.
func gpuInfo() []GPU {
	if gs := nvidiaSmiGPUs(); len(gs) > 0 {
		return gs
	}
	return wmiGPUs()
}

// wmiGPUs shells out to PowerShell's Get-CimInstance
// Win32_VideoController for adapter enumeration.
func wmiGPUs() []GPU {
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
		vram := r.AdapterRAM
		if wmiVRAMLooksCapped(vram) {
			// Unknown, not 4 GiB — see wmiVRAMLooksCapped.
			vram = 0
		}
		gpus = append(gpus, GPU{
			Kind:           gpuKindFromVendor(r.AdapterCompatibility),
			Model:          strings.TrimSpace(r.Name),
			Count:          1,
			VRAMTotalBytes: vram,
		})
	}
	return gpus
}
