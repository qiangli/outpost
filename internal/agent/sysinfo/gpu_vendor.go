package sysinfo

import "strings"

// GPU kind values reported in GPU.Kind. They are part of the
// cloudbox-outpost wire contract: vknode maps "apple-silicon" to the
// dhnt.io/metal-vram node resource and "nvidia" to nvidia.com/gpu, and
// the values also land in the outpost.dhnt.io/gpu-kind node label.
const (
	GPUKindApple  = "apple-silicon"
	GPUKindNVIDIA = "nvidia"
	GPUKindAMD    = "amd"
	GPUKindIntel  = "intel"
)

// gpuKindFromVendor maps a platform-reported vendor / manufacturer
// string to a canonical GPU kind, case-insensitively. Returns "" for a
// vendor we don't recognize — callers still report the model, so an
// unclassified adapter surfaces rather than disappearing.
//
// One shared implementation on purpose: each platform probe reads the
// vendor from a different source (lspci -mm on Linux,
// Win32_VideoController.AdapterCompatibility on Windows,
// system_profiler on darwin) but they all classify it the same way, and
// a per-platform copy is a copy that never gets tested — the Windows
// branch is not executed by CI at all.
//
// The AMD arm is deliberately NOT a bare Contains(v, "ati"). "ati" is a
// substring of "corporATIon", which nearly every vendor string carries
// ("Intel Corporation", "NVIDIA Corporation"), so a bare match ahead of
// the intel arm classified every Intel GPU as AMD — observed in the
// wild as outpost.dhnt.io/gpu-kind=amd on an Intel Comet Lake iGPU.
// Match the brand as it actually appears instead: lspci prints
// post-acquisition parts as "Advanced Micro Devices, Inc. [AMD/ATI]",
// and pre-acquisition ones as "ATI Technologies Inc".
func gpuKindFromVendor(vendor string) string {
	v := strings.ToLower(vendor)
	switch {
	case strings.Contains(v, "apple"):
		return GPUKindApple
	case strings.Contains(v, "nvidia"):
		return GPUKindNVIDIA
	case strings.Contains(v, "amd"),
		strings.Contains(v, "ati technologies"),
		strings.Contains(v, "advanced micro devices"):
		return GPUKindAMD
	case strings.Contains(v, "intel"):
		return GPUKindIntel
	default:
		return ""
	}
}
