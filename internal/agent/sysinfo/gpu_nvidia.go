package sysinfo

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// nvidiaSmiGPUs queries `nvidia-smi --query-gpu=name,memory.total
// --format=csv,noheader,nounits`. One row per GPU; memory in MiB.
// Returns nil when nvidia-smi isn't reachable or returns no rows
// (drivers loaded but no card visible).
//
// This lives in an untagged file — not sysinfo_linux.go — because
// Windows needs it too, and for a stronger reason than code reuse:
// the WMI path Windows would otherwise rely on CANNOT report the VRAM
// of a modern card (see wmiVRAMLooksCapped). nvidia-smi is the only
// source on Windows that tells the truth about an 8 GiB+ NVIDIA GPU,
// so it must be tried first there, exactly as it already is on Linux.
func nvidiaSmiGPUs() []GPU {
	bin, ok := lookNvidiaSmi()
	if !ok {
		return nil
	}
	out, err := exec.Command(bin,
		"--query-gpu=name,memory.total",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	gpus := []GPU{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
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
			Kind:           GPUKindNVIDIA,
			Model:          model,
			Count:          1,
			VRAMTotalBytes: memMiB * 1024 * 1024,
		})
	}
	return gpus
}

// lookNvidiaSmi resolves the nvidia-smi binary. PATH is the normal
// answer, but on Windows the driver installs it into System32 and a
// service started with a minimal environment may not have System32 on
// PATH — so we fall back to the known absolute location rather than
// silently reporting "no GPU" on a machine that plainly has one.
func lookNvidiaSmi() (string, bool) {
	if bin, err := exec.LookPath("nvidia-smi"); err == nil {
		return bin, true
	}
	if runtime.GOOS != "windows" {
		return "", false
	}
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	candidate := filepath.Join(root, "System32", "nvidia-smi.exe")
	if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
		return candidate, true
	}
	return "", false
}

// wmiVRAMCapBytes is the 4 GiB ceiling of Win32_VideoController's
// legacy uint32 AdapterRAM field.
const wmiVRAMCapBytes = uint64(4) * 1024 * 1024 * 1024

// wmiVRAMSlackBytes is how far below the cap a reading is still treated
// as "this is the cap, not a measurement". Observed in the field: an
// 8 GiB RTX 3070 reports 4293918720 — exactly 4 GiB minus 1 MiB, not
// the 4294967295 sentinel the naive check would look for. Allow a
// little room so near-cap variants are caught too.
const wmiVRAMSlackBytes = uint64(16) * 1024 * 1024

// wmiVRAMLooksCapped reports whether a Win32_VideoController AdapterRAM
// reading is indistinguishable from the field's 4 GiB ceiling.
//
// Such a value must be reported as UNKNOWN (0), never passed through as
// a measurement: it is wrong by 2x on an 8 GiB card and by 6x on a
// 24 GiB one, and every consumer downstream — the cloudbox VRAM-headroom
// router, the vknode dhnt.io/vram capacity, a shard's per-rank request —
// treats VRAMTotalBytes as trustworthy. A GPU whose size we cannot
// measure is strictly better modeled as "size unknown" than as "4 GiB":
// unknown declines to schedule, a lie schedules wrongly.
//
// The cost is that a genuine 4 GiB card also reads as unknown. That is
// the correct trade: under-claiming loses an opportunity, over-claiming
// loses the job. NVIDIA cards are unaffected in practice because
// nvidiaSmiGPUs answers first and answers correctly.
func wmiVRAMLooksCapped(bytes uint64) bool {
	return bytes >= wmiVRAMCapBytes-wmiVRAMSlackBytes && bytes <= wmiVRAMCapBytes
}
