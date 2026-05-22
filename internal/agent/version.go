package agent

import (
	"runtime/debug"
	"strings"
)

// BuildInfo describes the provenance of this outpost binary. Sourced from
// runtime/debug.ReadBuildInfo(); the Go toolchain stamps these settings
// automatically at `go build` time when the working directory is a VCS
// checkout, so no -ldflags injection is needed.
//
// Consumed by GET /version (full JSON) and embedded as a short string in
// GET /apps so cloudbox can surface "is this outpost up to date" in its
// /api/v1/hosts aggregate without a coordinated cloudbox change.
type BuildInfo struct {
	Commit    string `json:"commit"`             // full git sha1, empty if no VCS info
	VCSTime   string `json:"vcs_time,omitempty"` // ISO-8601 commit timestamp
	Dirty     bool   `json:"dirty"`              // true if working tree had uncommitted changes at build
	GoVersion string `json:"go_version"`         // e.g. "go1.25.0"
}

// Short returns a one-line human-readable identifier — 7-char commit, with
// a "-dirty" suffix when applicable. Falls back to "unknown" when the
// binary was built without VCS info (e.g. via `go run` or with
// -buildvcs=false). Suitable for embedding in /apps for cloudbox.
func (b BuildInfo) Short() string {
	if b.Commit == "" {
		return "unknown"
	}
	c := b.Commit
	if len(c) > 7 {
		c = c[:7]
	}
	if b.Dirty {
		return c + "-dirty"
	}
	return c
}

// ReadBuildInfo returns the build metadata embedded in the running binary.
// Returns zero values if debug.ReadBuildInfo fails (which it doesn't, for
// any normal `go build`-produced binary).
func ReadBuildInfo() BuildInfo {
	var b BuildInfo
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return b
	}
	b.GoVersion = info.GoVersion
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Commit = s.Value
		case "vcs.time":
			b.VCSTime = s.Value
		case "vcs.modified":
			b.Dirty = strings.EqualFold(s.Value, "true")
		}
	}
	return b
}
