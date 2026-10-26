package sysinfo

import (
	"strings"
)

// gpuKindFromVendorID maps PCI vendor IDs to canonical GPU kinds.
func gpuKindFromVendorID(vendorID uint32) string {
	switch vendorID {
	case 0x10DE:
		return GPUKindNVIDIA
	case 0x1002:
		return GPUKindAMD
	case 0x8086:
		return GPUKindIntel
	case 0x106B:
		return GPUKindApple
	default:
		return ""
	}
}

// isUnifiedWindowsGPU determines whether a Windows graphics adapter is an
// integrated GPU / APU using unified memory shared with system RAM.
func isUnifiedWindowsGPU(kind, model string, dedicated, shared uint64) bool {
	switch kind {
	case GPUKindApple:
		return true
	case GPUKindNVIDIA:
		return false
	case GPUKindIntel:
		// Discrete Intel Arc desktop cards (A380, A750, A770, B580) have dedicated VRAM >= 4 GiB
		m := strings.ToLower(model)
		if strings.Contains(m, "arc(tm) a") || strings.Contains(m, "arc(tm) b") {
			if dedicated >= 4*1024*1024*1024 {
				return false
			}
		}
		// Intel UHD, Iris, Iris Xe, HD Graphics are integrated
		return true
	case GPUKindAMD:
		m := strings.ToLower(model)
		// Discrete AMD cards: "Radeon RX 7900", "Radeon RX 6800", "Radeon Pro W", etc.
		if (strings.Contains(m, "rx ") || strings.Contains(m, "pro w")) && !strings.Contains(m, "graphics") {
			if dedicated >= 4*1024*1024*1024 {
				return false
			}
		}
		// Integrated AMD APU / graphics indicators:
		// "graphics", "8060s", "780m", "680m", "660m", "vega", "radeon(tm)", or dedicated == 0
		if strings.Contains(m, "graphics") || strings.Contains(m, "8060s") ||
			strings.Contains(m, "780m") || strings.Contains(m, "680m") ||
			strings.Contains(m, "660m") || strings.Contains(m, "vega") ||
			dedicated == 0 {
			return true
		}
		// If dedicated is small (e.g. BIOS aperture <= 512 MiB) while shared is larger
		if dedicated <= 512*1024*1024 || shared > dedicated {
			return true
		}
		return false
	default:
		// Unknown vendor: if dedicated VRAM is 0, or small aperture with larger shared budget
		if dedicated == 0 || (dedicated <= 512*1024*1024 && shared > dedicated) {
			return true
		}
		return false
	}
}

// classifyWindowsGPU normalizes a detected Windows graphics adapter into a sysinfo.GPU.
// It handles vendor classification, 4 GiB WMI cap bypass via registry or DXGI,
// and honest unified-memory iGPU shared budget reporting.
func classifyWindowsGPU(rawModel, rawVendor string, vendorID uint32, dedicated, shared, regSize uint64) GPU {
	model := strings.TrimSpace(rawModel)
	kind := gpuKindFromVendorID(vendorID)
	if kind == "" {
		kind = gpuKindFromVendor(rawVendor)
	}
	if kind == "" {
		kind = gpuKindFromVendor(model)
	}

	// Consult driver registry qwMemorySize if available and larger than dedicated
	if regSize > 0 && regSize > dedicated {
		dedicated = regSize
	}

	unified := isUnifiedWindowsGPU(kind, model, dedicated, shared)

	var vram uint64
	if unified {
		// For unified-memory iGPUs, report the honest memory budget:
		// dedicated pool if configured/measured, or shared system memory budget
		if dedicated > 0 && !wmiVRAMLooksCapped(dedicated) {
			vram = dedicated
		}
		if shared > vram {
			vram = shared
		}
	} else {
		// Discrete GPU: only dedicated VRAM counts
		if !wmiVRAMLooksCapped(dedicated) {
			vram = dedicated
		}
	}

	return GPU{
		Kind:           kind,
		Model:          model,
		Count:          1,
		VRAMTotalBytes: vram,
		UnifiedMemory:  unified,
	}
}
