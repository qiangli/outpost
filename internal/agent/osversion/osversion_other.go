//go:build !darwin && !linux && !windows

package osversion

// detect on other platforms (BSDs, Solaris, etc) — return empty so
// the caller falls back to runtime.GOOS. We could shell out to
// `uname -srm` here if there's a real BSD operator someday.
func detect() string { return "" }
