package sysinfo

import "testing"

func TestGPUKindFromVendorID(t *testing.T) {
	cases := []struct {
		id   uint32
		want string
	}{
		{0x10DE, GPUKindNVIDIA},
		{0x1002, GPUKindAMD},
		{0x8086, GPUKindIntel},
		{0x106B, GPUKindApple},
		{0x1414, ""},
		{0x0000, ""},
	}
	for _, c := range cases {
		if got := gpuKindFromVendorID(c.id); got != c.want {
			t.Errorf("gpuKindFromVendorID(0x%x) = %q, want %q", c.id, got, c.want)
		}
	}
}

func TestClassifyWindowsGPU_AMDUnifiedMemory(t *testing.T) {
	const (
		sixtyFourGiB  = 64 * 1024 * 1024 * 1024
		fortySevenGiB = 47 * 1024 * 1024 * 1024
	)
	// APU with unified memory: dedicated pool measured at 64 GiB, shared 47 GiB
	gpu := classifyWindowsGPU(
		"AMD Radeon(TM) 8060S Graphics",
		"Advanced Micro Devices, Inc.",
		0x1002,
		sixtyFourGiB,
		fortySevenGiB,
		sixtyFourGiB,
	)
	if gpu.Kind != GPUKindAMD {
		t.Errorf("Kind = %q, want %q", gpu.Kind, GPUKindAMD)
	}
	if !gpu.UnifiedMemory {
		t.Errorf("UnifiedMemory = false, want true for APU")
	}
	if gpu.VRAMTotalBytes != sixtyFourGiB {
		t.Errorf("VRAMTotalBytes = %d, want %d", gpu.VRAMTotalBytes, uint64(sixtyFourGiB))
	}
}

func TestClassifyWindowsGPU_AMDIAPUSharedBudget(t *testing.T) {
	const (
		fiveTwelveMiB = 512 * 1024 * 1024
		sixteenGiB    = 16 * 1024 * 1024 * 1024
	)
	// APU with tiny 512 MiB BIOS aperture, honest shared budget is 16 GiB
	gpu := classifyWindowsGPU(
		"AMD Radeon(TM) 780M Graphics",
		"Advanced Micro Devices, Inc.",
		0x1002,
		fiveTwelveMiB,
		sixteenGiB,
		0,
	)
	if gpu.Kind != GPUKindAMD {
		t.Errorf("Kind = %q, want %q", gpu.Kind, GPUKindAMD)
	}
	if !gpu.UnifiedMemory {
		t.Errorf("UnifiedMemory = false, want true for APU")
	}
	if gpu.VRAMTotalBytes != sixteenGiB {
		t.Errorf("VRAMTotalBytes = %d, want %d (shared budget)", gpu.VRAMTotalBytes, uint64(sixteenGiB))
	}
}

func TestClassifyWindowsGPU_IntelIGPUSharedBudget(t *testing.T) {
	const (
		oneTwentyEightMiB = 128 * 1024 * 1024
		eightGiB          = 8 * 1024 * 1024 * 1024
	)
	gpu := classifyWindowsGPU(
		"Intel(R) Iris(R) Xe Graphics",
		"Intel Corporation",
		0x8086,
		oneTwentyEightMiB,
		eightGiB,
		0,
	)
	if gpu.Kind != GPUKindIntel {
		t.Errorf("Kind = %q, want %q", gpu.Kind, GPUKindIntel)
	}
	if !gpu.UnifiedMemory {
		t.Errorf("UnifiedMemory = false, want true for Intel iGPU")
	}
	if gpu.VRAMTotalBytes != eightGiB {
		t.Errorf("VRAMTotalBytes = %d, want %d", gpu.VRAMTotalBytes, uint64(eightGiB))
	}
}

func TestClassifyWindowsGPU_DiscreteAMD(t *testing.T) {
	const (
		twentyGiB  = 20 * 1024 * 1024 * 1024
		sixteenGiB = 16 * 1024 * 1024 * 1024
	)
	// Discrete card with 20 GiB VRAM: shared system RAM must not be added
	gpu := classifyWindowsGPU(
		"AMD Radeon RX 7900 XT",
		"Advanced Micro Devices, Inc.",
		0x1002,
		twentyGiB,
		sixteenGiB,
		twentyGiB,
	)
	if gpu.Kind != GPUKindAMD {
		t.Errorf("Kind = %q, want %q", gpu.Kind, GPUKindAMD)
	}
	if gpu.UnifiedMemory {
		t.Errorf("UnifiedMemory = true, want false for discrete card")
	}
	if gpu.VRAMTotalBytes != twentyGiB {
		t.Errorf("VRAMTotalBytes = %d, want %d", gpu.VRAMTotalBytes, uint64(twentyGiB))
	}
}

func TestClassifyWindowsGPU_DiscreteNVIDIA(t *testing.T) {
	const (
		eightGiB   = 8 * 1024 * 1024 * 1024
		sixteenGiB = 16 * 1024 * 1024 * 1024
	)
	gpu := classifyWindowsGPU(
		"NVIDIA GeForce RTX 3070",
		"NVIDIA",
		0x10DE,
		eightGiB,
		sixteenGiB,
		eightGiB,
	)
	if gpu.Kind != GPUKindNVIDIA {
		t.Errorf("Kind = %q, want %q", gpu.Kind, GPUKindNVIDIA)
	}
	if gpu.UnifiedMemory {
		t.Errorf("UnifiedMemory = true, want false for NVIDIA discrete card")
	}
	if gpu.VRAMTotalBytes != eightGiB {
		t.Errorf("VRAMTotalBytes = %d, want %d", gpu.VRAMTotalBytes, uint64(eightGiB))
	}
}

func TestClassifyWindowsGPU_WMICappedBypassedByRegistry(t *testing.T) {
	const (
		cappedWMI    = uint64(4293918720) // 4 GiB - 1 MiB cap
		sixtyFourGiB = 64 * 1024 * 1024 * 1024
	)
	// When WMI reports the capped 4 GiB, but the driver registry reports 64 GiB
	gpu := classifyWindowsGPU(
		"AMD Radeon(TM) 8060S Graphics",
		"Advanced Micro Devices, Inc.",
		0,
		cappedWMI,
		0,
		sixtyFourGiB,
	)
	if gpu.VRAMTotalBytes != sixtyFourGiB {
		t.Errorf("VRAMTotalBytes = %d, want %d (recovered from registry)", gpu.VRAMTotalBytes, uint64(sixtyFourGiB))
	}
	if !gpu.UnifiedMemory {
		t.Errorf("UnifiedMemory = false, want true")
	}
}
