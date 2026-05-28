//go:build linux

package sysinfo

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// memTotalBytes scrapes /proc/meminfo for "MemTotal: <kB>". Returns
// bytes (kB * 1024). Zero on any parse failure — caller omits the
// field then.
func memTotalBytes() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		// Format: "MemTotal:       16335952 kB"
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

// cpuModel returns the first "model name" line from /proc/cpuinfo.
// On Apple Silicon Linux VMs and most x86 boxes this is the marketing
// string; on bare ARM SoCs it may be missing — empty return is OK.
func cpuModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "model name") {
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			return ""
		}
		return strings.TrimSpace(line[idx+1:])
	}
	return ""
}

// gpuInfo enumerates GPUs in priority order: NVIDIA (nvidia-smi)
// first because it gives us VRAM directly; AMD ROCm next via
// rocm-smi; lspci as the catch-all that at least surfaces vendor +
// model without VRAM (Intel iGPU, AMD without ROCm, embedded SoCs).
//
// Probes that fail (binary missing, not in PATH, no driver loaded)
// are silent — we continue to the next probe. Returning an empty
// list = "no GPU detected"; the cloudbox routing layer treats that
// as CPU-only.
func gpuInfo() []GPU {
	if gs := nvidiaSmiGPUs(); len(gs) > 0 {
		return gs
	}
	if gs := rocmSmiGPUs(); len(gs) > 0 {
		return gs
	}
	return lspciGPUs()
}

// nvidiaSmiGPUs queries `nvidia-smi --query-gpu=name,memory.total
// --format=csv,noheader,nounits`. One row per GPU; memory in MiB.
// Returns nil when nvidia-smi isn't on PATH or returns no rows
// (drivers loaded but no card visible).
func nvidiaSmiGPUs() []GPU {
	bin, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil
	}
	out, err := exec.Command(bin,
		"--query-gpu=name,memory.total",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	gpus := []GPU{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 2 {
			continue
		}
		model := strings.TrimSpace(fields[0])
		memMiB, err := strconv.ParseUint(strings.TrimSpace(fields[1]), 10, 64)
		if err != nil {
			continue
		}
		gpus = append(gpus, GPU{
			Kind:           "nvidia",
			Model:          model,
			Count:          1,
			VRAMTotalBytes: memMiB * 1024 * 1024,
		})
	}
	return gpus
}

// rocmSmiGPUs queries `rocm-smi --showproductname --showmeminfo vram
// --json`. Output shape: `{"card0":{"Card Series": "...", "VRAM
// Total Memory (B)": "..."}, ...}`. Returns nil when rocm-smi is
// missing.
func rocmSmiGPUs() []GPU {
	bin, err := exec.LookPath("rocm-smi")
	if err != nil {
		return nil
	}
	out, err := exec.Command(bin,
		"--showproductname", "--showmeminfo", "vram", "--json").Output()
	if err != nil {
		return nil
	}
	// rocm-smi's JSON keys vary across versions; parse defensively by
	// looking for substring patterns rather than rigid schema.
	gpus := []GPU{}
	// Each "cardN" is a top-level key — scan loosely.
	for _, line := range strings.Split(string(out), "\n") {
		l := strings.ToLower(line)
		switch {
		case strings.Contains(l, "card series") || strings.Contains(l, "card model"):
			if idx := strings.Index(line, ":"); idx > 0 {
				model := strings.Trim(strings.TrimSpace(line[idx+1:]), `",`)
				if model != "" {
					gpus = append(gpus, GPU{Kind: "amd", Model: model, Count: 1})
				}
			}
		case strings.Contains(l, "vram total memory") && len(gpus) > 0:
			if idx := strings.Index(line, ":"); idx > 0 {
				raw := strings.Trim(strings.TrimSpace(line[idx+1:]), `",`)
				if n, err := strconv.ParseUint(raw, 10, 64); err == nil {
					gpus[len(gpus)-1].VRAMTotalBytes = n
				}
			}
		}
	}
	return gpus
}

// lspciGPUs is the lowest-precision fallback: parses `lspci` output
// for VGA/3D/Display rows. No VRAM, but at least surfaces vendor +
// model so cloudbox can tell "this outpost has Intel UHD" apart from
// "this outpost is headless / no display adapter at all". Returns
// empty when lspci is missing.
func lspciGPUs() []GPU {
	bin, err := exec.LookPath("lspci")
	if err != nil {
		return nil
	}
	out, err := exec.Command(bin, "-mm").Output()
	if err != nil {
		return nil
	}
	gpus := []GPU{}
	for _, line := range strings.Split(string(out), "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "vga") &&
			!strings.Contains(lower, "3d controller") &&
			!strings.Contains(lower, "display") {
			continue
		}
		// `-mm` quotes vendor + model fields. Crude but stable across
		// distros — extract the first two quoted strings after the
		// device class.
		fields := lspciSplit(line)
		if len(fields) < 4 {
			continue
		}
		vendor := strings.ToLower(fields[2])
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
			Kind:  kind,
			Model: fields[3],
			Count: 1,
		})
	}
	return gpus
}

// lspciSplit splits an `lspci -mm` line into its quoted+unquoted
// fields. Format: "00:02.0 "VGA" "Intel" "UHD Graphics" "rev 01"".
// We only need indices 0..3 (slot, class, vendor, model).
func lspciSplit(line string) []string {
	var fields []string
	in := strings.NewReader(line)
	var cur strings.Builder
	inQuote := false
	for {
		r, _, err := in.ReadRune()
		if err != nil {
			break
		}
		switch {
		case r == '"':
			inQuote = !inQuote
			if !inQuote {
				fields = append(fields, cur.String())
				cur.Reset()
			}
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				fields = append(fields, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		fields = append(fields, cur.String())
	}
	return fields
}
