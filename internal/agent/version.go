package agent

import (
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/osversion"
)

// daemonStartedAt is captured at process start by an init() so the
// /apps poller can surface "running for N hours" without any threading
// from main.go. Stable for the life of the process.
var daemonStartedAt = time.Now()

// releaseTag is populated at link time via -ldflags:
//
//	go build -ldflags "-X github.com/qiangli/outpost/internal/agent.releaseTag=v0.2.0" ./cmd/outpost
//
// Empty for ad-hoc `go build` / `go run` invocations and dirty
// working-tree builds. The release GH Action sets it from the
// triggering git tag; cloudbox compares tags to highlight "update
// available" without having to decode opaque commit shas.
var releaseTag string

// BuildInfo describes the provenance of this outpost binary. Sourced
// from runtime/debug.ReadBuildInfo() (commit, vcs_time, dirty,
// go_version are stamped automatically by `go build` in a VCS
// checkout), plus Version which is ldflags-injected at release-tag
// build time, plus OS/Arch from runtime.GOOS/GOARCH so cloudbox knows
// which platform artifact to push when fan-out-rolling the fleet.
//
// Consumed by GET /version (full JSON) and embedded as a short string
// in GET /apps so cloudbox can surface "is this outpost up to date"
// without a coordinated cloudbox change.
type BuildInfo struct {
	Version   string `json:"version,omitempty"`  // semver tag, e.g. "v0.2.0"; empty for untagged builds
	Commit    string `json:"commit"`             // full git sha1, empty if no VCS info
	VCSTime   string `json:"vcs_time,omitempty"` // ISO-8601 commit timestamp
	Dirty     bool   `json:"dirty"`              // true if working tree had uncommitted changes at build
	GoVersion string `json:"go_version"`         // e.g. "go1.26.0"
	OS        string `json:"os,omitempty"`       // runtime.GOOS — "darwin" / "linux" / "windows" (compile target)
	Arch      string `json:"arch,omitempty"`     // runtime.GOARCH — "arm64" / "amd64"

	// OSVersion is the actual host OS at RUNTIME (sw_vers / /etc/
	// os-release / cmd ver), e.g. "macOS 15.1.0" / "Ubuntu 24.04
	// LTS". Distinct from OS above which is the binary's compile
	// target — they typically match but can disagree if a binary
	// is shipped cross-OS (mostly an alert that something's
	// misconfigured).
	OSVersion string `json:"os_version,omitempty"`

	// BinarySize is the on-disk size of os.Executable() in bytes.
	// Useful for the SPA host row's at-a-glance "is the binary the
	// expected ballpark size or is something truncated."
	BinarySize int64 `json:"binary_size,omitempty"`

	// InstalledAt is the mtime of os.Executable() — when the binary
	// file was last written to disk. Reflects the most recent
	// upgrade/install (the daemon's previous swap or the operator's
	// scp), NOT the daemon's process start.
	InstalledAt time.Time `json:"installed_at,omitempty"`

	// DaemonStartedAt is the process-start timestamp captured at
	// the first ReadBuildInfo call via the package-level var. Stable
	// for the life of this daemon.
	DaemonStartedAt time.Time `json:"daemon_started_at,omitempty"`
}

// Short returns a one-line human-readable identifier. Prefers the
// semver tag when present (e.g. "v0.2.0"); falls back to the 7-char
// commit with a "-dirty" suffix when applicable; "unknown" when the
// binary was built without VCS info (e.g. via `go run` or with
// -buildvcs=false). Suitable for embedding in /apps for cloudbox.
func (b BuildInfo) Short() string {
	if b.Version != "" && !b.Dirty {
		return b.Version
	}
	if b.Commit == "" {
		if b.Version != "" {
			return b.Version + "-dirty"
		}
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

// ReadBuildInfo returns the build metadata embedded in the running
// binary. Returns zero values for the VCS fields if debug.ReadBuildInfo
// fails (which it doesn't for any normal `go build`-produced binary).
func ReadBuildInfo() BuildInfo {
	b := BuildInfo{
		Version:         releaseTag,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		OSVersion:       osversion.String(),
		DaemonStartedAt: daemonStartedAt,
	}
	if exe, err := os.Executable(); err == nil {
		if st, err := os.Stat(exe); err == nil {
			b.BinarySize = st.Size()
			b.InstalledAt = st.ModTime()
		}
	}
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
