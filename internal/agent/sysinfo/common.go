//go:build darwin || linux

package sysinfo

import (
	"os"
	"syscall"
)

// hostname returns os.Hostname or "" on failure. Trimmed to the
// short form on POSIX hostnames; we don't try to resolve FQDN.
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// diskUsageBytes uses statfs to read total + free bytes for the
// filesystem containing path. Returns (0, 0) on any error so the
// caller can decide whether to omit the field.
//
// Bsize semantics differ across kernels; Statfs_t.Frsize is what
// you want for "fragment size" on linux but darwin only sets Bsize
// — we use Bsize uniformly since both kernels report it.
func diskUsageBytes(path string) (total, free uint64) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, 0
	}
	total = uint64(s.Blocks) * uint64(s.Bsize)
	free = uint64(s.Bavail) * uint64(s.Bsize)
	return total, free
}
