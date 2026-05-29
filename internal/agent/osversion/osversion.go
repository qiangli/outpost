// Package osversion returns a one-line human-readable OS version
// label for the running host, e.g. "macOS 15.1.0" / "Ubuntu 24.04
// LTS" / "Windows 11 Pro 26100". Distinct from the compile-time
// runtime.GOOS ("darwin" / "linux" / "windows") in two important ways:
//
//  1. It reflects the actual host OS at runtime, not the binary's
//     build target. A darwin binary scp'd to a linux box would still
//     report runtime.GOOS=="darwin" but couldn't actually start there;
//     this package gives cloudbox the real story.
//  2. It includes the version/distribution, useful for the SPA's
//     "running where" display ("macOS 15.1" beats "darwin").
//
// Cached on first call — host OS version doesn't change at process
// lifetime, and the underlying calls (sw_vers / /etc/os-release / ver)
// are exec/IO heavy enough that we don't want to repeat them per poll.
package osversion

import (
	"runtime"
	"sync"
)

var (
	once   sync.Once
	cached string
)

// String returns the cached one-line OS version label. Falls back to
// runtime.GOOS when the platform-specific lookup fails or isn't
// implemented (BSDs, Solaris, etc).
func String() string {
	once.Do(func() {
		cached = detect()
		if cached == "" {
			cached = runtime.GOOS
		}
	})
	return cached
}
