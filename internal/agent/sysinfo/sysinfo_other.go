//go:build !darwin && !linux && !windows

package sysinfo

// Stubs for platforms we don't ship to (freebsd, openbsd, etc.).
// Returns zero values so Collect() still produces a usable struct
// minus the platform-specific bits — the receiving cloudbox just
// shows "—" for unset fields. Windows + darwin + linux have their
// own files; this is the bin for everything else.

import "os"

func memTotalBytes() uint64                      { return 0 }
func cpuModel() string                           { return "" }
func diskUsageBytes(string) (total, free uint64) { return 0, 0 }
func gpuInfo() []GPU                             { return nil }
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}
