// Package sysinfo collects host capability information the outpost
// reports to cloudbox via the /apps poll loop. Cloudbox stores the
// result on the HostEntry and surfaces it in the UI; future LB
// policies can use the data (e.g. preferentially place CPU-heavy
// pods on outposts with more cores).
//
// Pure stdlib — no gopsutil dep. Per-platform helpers live in
// sysinfo_{darwin,linux,other}.go behind build tags.
package sysinfo

import (
	"runtime"
)

// Info is the wire shape outpost ships in the /apps response under
// the `system` key. All sizes in bytes for forward compatibility;
// cloudbox renders them in friendlier units. Fields are
// omitemptied so legacy outposts (running before this struct
// landed) don't surface as "0 MB" on the cloudbox UI.
type Info struct {
	// Arch + OS duplicate the BuildInfo fields but are useful here
	// because cloudbox renders this struct as one block; saves the
	// caller from wiring two sources together.
	Arch string `json:"arch,omitempty"`
	OS   string `json:"os,omitempty"`

	// CPUCount is logical CPUs as the Go runtime sees them.
	CPUCount int `json:"cpu_count,omitempty"`

	// CPUModel is best-effort: sysctl machdep.cpu.brand_string on
	// darwin, /proc/cpuinfo's "model name" on linux. Empty when
	// unavailable.
	CPUModel string `json:"cpu_model,omitempty"`

	// MemTotalBytes is total physical memory the kernel reports.
	MemTotalBytes uint64 `json:"mem_total_bytes,omitempty"`

	// DiskTotalBytes / DiskFreeBytes describe the filesystem
	// hosting the outpost data dir (the cache dir we pass in).
	// The cluster mode's image cache + node DataDir live here, so
	// it's the practical "how much room does this outpost have for
	// pods" number.
	DiskTotalBytes uint64 `json:"disk_total_bytes,omitempty"`
	DiskFreeBytes  uint64 `json:"disk_free_bytes,omitempty"`

	// Hostname is what the OS reports as the machine name —
	// informational only; the cloudbox-side host key is
	// agent_name, not this.
	Hostname string `json:"hostname,omitempty"`

	// GPUs is the list of accelerator devices we managed to detect.
	// Each entry is one physical (or logical, for Apple Silicon) GPU.
	// Empty list = "no GPU detected or the platform probe failed" —
	// downstream consumers (cloudbox's LLM pool router) treat this as
	// CPU-only.
	//
	// Detection is best-effort and probe-based: we shell out to the
	// platform's standard GPU enumeration tool (system_profiler on
	// macOS, nvidia-smi/lspci on Linux, PowerShell Get-CimInstance on
	// Windows). Outposts running in environments where those tools
	// aren't available or refuse to enumerate (locked-down containers,
	// no driver loaded) ship an empty list, not an error — the
	// /apps endpoint stays healthy.
	GPUs []GPU `json:"gpus,omitempty"`
}

// GPU is one accelerator device the outpost detected. Designed to be
// minimal but sufficient for routing decisions: the cloudbox-side
// LLM pool router prefers GPU-equipped outposts and (Phase 1) ranks
// candidates by VRAM headroom for a given model size.
//
// Kind taxonomy (stable; new values are additive):
//   - "apple-silicon"  — M-series unified-memory GPUs
//   - "nvidia"         — CUDA-capable NVIDIA cards
//   - "amd"            — ROCm-capable AMD cards
//   - "intel"          — Intel integrated / Arc discrete
//   - "metal"          — Intel Mac with a non-Apple-Silicon GPU
//   - ""               — vendor unknown
//
// VRAMTotalBytes carries the total VRAM the device reports. On Apple
// Silicon (UnifiedMemory=true) this equals system MemTotalBytes —
// the OS shares one pool between CPU and GPU. Consumers must apply
// their own headroom policy (MLX defaults to ~70% of unified memory
// being safely usable for model weights).
type GPU struct {
	Kind           string `json:"kind,omitempty"`
	Model          string `json:"model,omitempty"`
	Count          int    `json:"count,omitempty"`
	VRAMTotalBytes uint64 `json:"vram_total_bytes,omitempty"`
	UnifiedMemory  bool   `json:"unified_memory,omitempty"`
}

// Collect populates an Info struct against the current process's
// view of the host. dataDir is the path the disk-usage probe
// targets (typically the outpost cache dir). Pass "" to skip the
// disk probe — useful for tests.
func Collect(dataDir string) Info {
	i := Info{
		Arch:     runtime.GOARCH,
		OS:       runtime.GOOS,
		CPUCount: runtime.NumCPU(),
		Hostname: hostname(),
		CPUModel: cpuModel(),
	}
	if m := memTotalBytes(); m > 0 {
		i.MemTotalBytes = m
	}
	if dataDir != "" {
		if total, free := diskUsageBytes(dataDir); total > 0 {
			i.DiskTotalBytes = total
			i.DiskFreeBytes = free
		}
	}
	if gs := gpuInfo(); len(gs) > 0 {
		i.GPUs = gs
	}
	return i
}
