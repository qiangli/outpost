//go:build windows

package sysinfo

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
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
// over DXGI, registry, and WMI.
//
// Probe priority:
//  1. nvidia-smi: direct vendor tool for NVIDIA hardware.
//  2. DXGI: user-mode DirectX Graphics Infrastructure enumerates adapters,
//     retrieves true dedicated and shared video memory beyond WMI's 4 GiB
//     ceiling, and consults driver registry qwMemorySize when available.
//  3. Driver Registry: direct scan of the display adapter class key
//     (HKLM\SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318})
//     for HardwareInformation.qwMemorySize.
//  4. WMI: fallback Win32_VideoController query, paired with registry
//     enrichment so capped readings are unmasked whenever possible.
func gpuInfo() []GPU {
	if gs := nvidiaSmiGPUs(); len(gs) > 0 {
		return gs
	}
	if gs := dxgiGPUs(); len(gs) > 0 {
		return gs
	}
	if gs := registryGPUs(); len(gs) > 0 {
		return gs
	}
	return wmiGPUs()
}

var (
	modDXGI                = syscall.NewLazyDLL("dxgi.dll")
	procCreateDXGIFactory1 = modDXGI.NewProc("CreateDXGIFactory1")
	procCreateDXGIFactory  = modDXGI.NewProc("CreateDXGIFactory")

	iidIDXGIFactory1 = syscall.GUID{
		Data1: 0x770aae78,
		Data2: 0xf26f,
		Data3: 0x4dba,
		Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87},
	}
	iidIDXGIFactory = syscall.GUID{
		Data1: 0x7b716634,
		Data2: 0x2142,
		Data3: 0x4433,
		Data4: [8]byte{0xa3, 0x20, 0x24, 0x4f, 0xe5, 0x3e, 0xbf, 0xf0},
	}
)

type dxgiAdapterDesc1 struct {
	Description           [128]uint16
	VendorId              uint32
	DeviceId              uint32
	SubSysId              uint32
	Revision              uint32
	DedicatedVideoMemory  uintptr
	DedicatedSystemMemory uintptr
	SharedSystemMemory    uintptr
	AdapterLuid           int64
	Flags                 uint32
}

const dxgiAdapterFlagSoftware = 2

type comObject struct {
	vtbl *[25]uintptr
}

func releaseCOM(obj *comObject) {
	if obj == nil {
		return
	}
	syscall.SyscallN(obj.vtbl[2], uintptr(unsafe.Pointer(obj)))
}

// registryMemorySizes scans the display adapter device class registry
// for HardwareInformation.qwMemorySize values, returning a map keyed by
// lowercase driver description and device identifier.
func registryMemorySizes() map[string]uint64 {
	m := make(map[string]uint64)
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`, registry.READ)
	if err != nil {
		return m
	}
	defer k.Close()

	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return m
	}
	for _, name := range names {
		if len(name) != 4 {
			continue
		}
		sub, err := registry.OpenKey(k, name, registry.READ)
		if err != nil {
			continue
		}
		qw, _, errQW := sub.GetIntegerValue("HardwareInformation.qwMemorySize")
		desc, _, errDesc := sub.GetStringValue("DriverDesc")
		if errDesc == nil && errQW == nil && qw > 0 {
			m[strings.ToLower(strings.TrimSpace(desc))] = qw
		}
		matching, _, errMatching := sub.GetStringValue("MatchingDeviceId")
		if errMatching == nil && errQW == nil && qw > 0 {
			m[strings.ToLower(strings.TrimSpace(matching))] = qw
		}
		sub.Close()
	}
	return m
}

// dxgiGPUs queries display adapters using DirectX Graphics Infrastructure.
func dxgiGPUs() []GPU {
	regSizes := registryMemorySizes()

	var factory *comObject
	if err := procCreateDXGIFactory1.Find(); err == nil {
		hr, _, _ := procCreateDXGIFactory1.Call(
			uintptr(unsafe.Pointer(&iidIDXGIFactory1)),
			uintptr(unsafe.Pointer(&factory)),
		)
		if hr != 0 {
			factory = nil
		}
	}
	if factory == nil {
		if err := procCreateDXGIFactory.Find(); err == nil {
			hr, _, _ := procCreateDXGIFactory.Call(
				uintptr(unsafe.Pointer(&iidIDXGIFactory)),
				uintptr(unsafe.Pointer(&factory)),
			)
			if hr != 0 {
				return nil
			}
		} else {
			return nil
		}
	}
	defer releaseCOM(factory)

	enumAdapters1 := factory.vtbl[12]

	var gpus []GPU
	for i := uint32(0); ; i++ {
		var adapter *comObject
		hr, _, _ := syscall.SyscallN(enumAdapters1, uintptr(unsafe.Pointer(factory)), uintptr(i), uintptr(unsafe.Pointer(&adapter)))
		if hr != 0 || adapter == nil {
			break
		}

		getDesc1 := adapter.vtbl[10]
		var desc dxgiAdapterDesc1
		hr, _, _ = syscall.SyscallN(getDesc1, uintptr(unsafe.Pointer(adapter)), uintptr(unsafe.Pointer(&desc)))
		releaseCOM(adapter)

		if hr != 0 {
			continue
		}

		// Filter out software / emulated adapters (e.g. WARP, Microsoft Basic Render Driver).
		if desc.Flags&dxgiAdapterFlagSoftware != 0 || desc.VendorId == 0x1414 {
			continue
		}

		model := strings.TrimSpace(syscall.UTF16ToString(desc.Description[:]))
		if model == "" {
			continue
		}

		dedicated := uint64(desc.DedicatedVideoMemory)
		shared := uint64(desc.SharedSystemMemory)
		regSize := regSizes[strings.ToLower(model)]

		g := classifyWindowsGPU(model, "", desc.VendorId, dedicated, shared, regSize)
		gpus = append(gpus, g)
	}
	return gpus
}

// registryGPUs enumerates hardware display adapters directly from the
// Windows display class driver registry.
func registryGPUs() []GPU {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`, registry.READ)
	if err != nil {
		return nil
	}
	defer k.Close()

	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil
	}

	var gpus []GPU
	for _, name := range names {
		if len(name) != 4 {
			continue
		}
		sub, err := registry.OpenKey(k, name, registry.READ)
		if err != nil {
			continue
		}
		desc, _, errDesc := sub.GetStringValue("DriverDesc")
		provider, _, _ := sub.GetStringValue("ProviderName")
		qw, _, errQW := sub.GetIntegerValue("HardwareInformation.qwMemorySize")
		dw, _, errDW := sub.GetIntegerValue("HardwareInformation.MemorySize")
		sub.Close()

		if errDesc != nil || strings.TrimSpace(desc) == "" {
			continue
		}
		model := strings.TrimSpace(desc)
		low := strings.ToLower(model)
		if strings.Contains(low, "basic render") || strings.Contains(low, "remote desktop") {
			continue
		}

		var dedicated uint64
		if errQW == nil && qw > 0 {
			dedicated = qw
		} else if errDW == nil && dw > 0 && !wmiVRAMLooksCapped(dw) {
			dedicated = dw
		}

		g := classifyWindowsGPU(model, provider, 0, dedicated, 0, qw)
		gpus = append(gpus, g)
	}
	return gpus
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
	regSizes := registryMemorySizes()
	gpus := make([]GPU, 0, len(rows))
	for _, r := range rows {
		name := strings.TrimSpace(r.Name)
		regSize := regSizes[strings.ToLower(name)]
		g := classifyWindowsGPU(name, r.AdapterCompatibility, 0, r.AdapterRAM, 0, regSize)
		gpus = append(gpus, g)
	}
	return gpus
}
