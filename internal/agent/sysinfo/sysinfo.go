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
	return i
}
