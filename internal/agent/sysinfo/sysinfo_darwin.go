//go:build darwin

package sysinfo

import (
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
)

// memTotalBytes reads `sysctl -n hw.memsize` which the kernel
// returns as bytes (uint64). Avoids CGo by shelling out — sysctl
// is in /usr/sbin which is always on the default PATH.
func memTotalBytes() uint64 {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// cpuModel returns the marketing brand string Apple Silicon /
// Intel both ship via sysctl. Trimmed of the trailing newline.
// "" when sysctl is unavailable or returns nothing useful.
func cpuModel() string {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "machdep.cpu.brand_string").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gpuInfo enumerates display adapters via `system_profiler
// SPDisplaysDataType -json`. On Apple Silicon the GPU shows up as
// part of the integrated SoC; on Intel Macs with discrete cards we'd
// get multiple entries. VRAM on Apple Silicon is reported as
// "Apple_Default" or similar (unified memory); we fall back to
// MemTotalBytes in that case so consumers have a number to budget
// against.
//
// The system_profiler binary is always present on macOS (part of the
// OS) so a failure here means the JSON shape changed across OS
// releases or the user is running on a heavily sandboxed
// configuration. Either way: log nothing, return empty, let the
// /apps endpoint stay green.
func gpuInfo() []GPU {
	// -detailLevel mini cuts the output to what we need (vendor +
	// model + VRAM string) without dumping the full SPI surface.
	cmd := exec.Command("/usr/sbin/system_profiler", "SPDisplaysDataType",
		"-json", "-detailLevel", "mini")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var payload struct {
		Displays []struct {
			Name      string `json:"_name"`
			Vendor    string `json:"spdisplays_vendor"`
			VRAMSPD   string `json:"_spdisplays_vram_shared"`
			VRAMOther string `json:"spdisplays_vram"`
		} `json:"SPDisplaysDataType"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil
	}
	if len(payload.Displays) == 0 {
		return nil
	}
	mem := memTotalBytes()
	gpus := make([]GPU, 0, len(payload.Displays))
	for _, d := range payload.Displays {
		g := GPU{
			Model: strings.TrimSpace(d.Name),
			Count: 1,
		}
		// Apple Silicon: vendor string contains "apple" or
		// "sppci_vendor_Apple". Unified memory ⇒ no discrete
		// VRAM number, share system memory.
		v := strings.ToLower(d.Vendor)
		switch {
		case strings.Contains(v, "apple"):
			g.Kind = "apple-silicon"
			g.UnifiedMemory = true
			g.VRAMTotalBytes = mem
		case strings.Contains(v, "nvidia"):
			g.Kind = "nvidia"
		case strings.Contains(v, "amd") || strings.Contains(v, "ati"):
			g.Kind = "amd"
		case strings.Contains(v, "intel"):
			g.Kind = "intel"
		case strings.Contains(strings.ToLower(d.Name), "apple"):
			// Older OS revisions stash the brand only in _name.
			g.Kind = "apple-silicon"
			g.UnifiedMemory = true
			g.VRAMTotalBytes = mem
		}
		// Discrete-VRAM string: "8 GB" / "1536 MB" / "8192 MB" —
		// system_profiler chose a unit dynamically. Parse if
		// present so dGPU Intel Macs report a real number.
		if !g.UnifiedMemory {
			if n := parseVRAMString(firstNonEmpty(d.VRAMOther, d.VRAMSPD)); n > 0 {
				g.VRAMTotalBytes = n
			}
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// parseVRAMString accepts the human-readable VRAM strings
// system_profiler emits — "8 GB", "1536 MB", "8192 MB" — and returns
// the corresponding byte count. Returns 0 on any parse failure.
func parseVRAMString(s string) uint64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	parts := strings.Fields(s)
	if len(parts) < 2 {
		return 0
	}
	n, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0
	}
	switch strings.ToUpper(parts[1]) {
	case "GB":
		return n * 1024 * 1024 * 1024
	case "MB":
		return n * 1024 * 1024
	case "KB":
		return n * 1024
	}
	return 0
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
