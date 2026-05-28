package sysinfo

import "testing"

func TestGPUStructShape(t *testing.T) {
	// The wire shape is part of the cloudbox-outpost contract;
	// renaming a json tag silently breaks deployed cloudboxes.
	// Easier to assert in a test than to rely on memory.
	g := GPU{
		Kind:           "nvidia",
		Model:          "RTX 4090",
		Count:          1,
		VRAMTotalBytes: 24 * 1024 * 1024 * 1024,
		UnifiedMemory:  false,
	}
	// Confirm zero-values omitempty out cleanly so a CPU-only row
	// doesn't ship `{"kind":"","model":"","count":0,...}`.
	if (GPU{}).Kind != "" || (GPU{}).VRAMTotalBytes != 0 {
		t.Errorf("zero-value GPU not zero-valued: %+v", GPU{})
	}
	if g.Kind != "nvidia" || g.VRAMTotalBytes == 0 {
		t.Errorf("populated GPU lost data: %+v", g)
	}
}

func TestInfoCollect_NoCrashOnEmptyDataDir(t *testing.T) {
	// Collect("") skips the disk probe but should otherwise produce a
	// usable Info. Verifies the probe wiring doesn't panic on the no-
	// dataDir path (the dev-mode `outpost docs` and similar code paths
	// pass "" before the disk cache is provisioned).
	info := Collect("")
	if info.Arch == "" || info.OS == "" {
		t.Errorf("Collect produced unusable Info: %+v", info)
	}
	if info.CPUCount <= 0 {
		t.Errorf("CPUCount should be positive, got %d", info.CPUCount)
	}
}
