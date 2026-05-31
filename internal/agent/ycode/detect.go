// Package ycode discovers and lifecycle-manages a `ycode serve`
// process running side-by-side with outpost on the same OS user
// account. ycode is the under-the-hood agentic engine outpost
// delegates to for inference (Ollama embedded), container management
// (podman embedded), Gitea, OTel, and similar; it's optional and
// distributed as a separate binary.
//
// Discovery model matches ycode's own TUI: a running `ycode serve`
// publishes a manifest at $HOME/.agents/ycode/manifest.json + a
// matching server.token. Outpost reads the manifest, health-checks
// the api endpoint, and reports the result so the admin UI can
// decide whether to show "Running", "Install ycode", or
// "installed (not running)" hints.
//
// Detection-only — outpost never spawns or restarts `ycode serve`.
// The operator owns ycode's lifecycle (it may carry flags outpost
// has no way to reproduce); we just report what we see.
package ycode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// State is what Detect reports. Mutually exclusive. The admin UI
// chooses one of three render shapes off this.
type State string

const (
	// StateRunning: manifest file present, server.token present, api
	// endpoint responded to a probe.
	StateRunning State = "running"

	// StateStaleManifest: manifest file present but the api endpoint
	// did not respond. The previous ycode process is gone but left
	// its manifest behind. Caller can clean up and treat as
	// StateInstalled.
	StateStaleManifest State = "stale_manifest"

	// StateInstalled: no manifest, but a `ycode` binary is on PATH or
	// in a known install location. Admin UI tells the operator to
	// run `ycode serve` themselves — outpost does not spawn.
	StateInstalled State = "installed"

	// StateNotInstalled: no manifest, no `ycode` binary anywhere we
	// look. Admin UI surfaces a download link to the GitHub releases
	// page (StateDownloadURL).
	StateNotInstalled State = "not_installed"
)

// Info is the full status surface Detect returns. Always populated
// regardless of state so the admin UI can render every field
// (download URL, manifest path) even when the corresponding bit is
// inactive — operators benefit from seeing "we checked here and it
// wasn't there."
type Info struct {
	State State `json:"state"`

	// BinaryPath is the absolute path to the ycode binary we found,
	// or "" when StateNotInstalled. Resolved via LookPath +
	// $HOME/bin fallback in that order.
	BinaryPath string `json:"binary_path,omitempty"`

	// ManifestPath is where we look for ycode's discovery manifest.
	// Stable across runs ($HOME/.agents/ycode/manifest.json). Shown
	// to operators who need to clean up a stale manifest by hand.
	ManifestPath string `json:"manifest_path"`

	// APIEndpoint is the URL from manifest.endpoints.api, when the
	// manifest was readable. Empty when no manifest. Includes the
	// trailing path segment (e.g. "http://127.0.0.1:31415/ycode/").
	APIEndpoint string `json:"api_endpoint,omitempty"`

	// Version is what `ycode version --short` reports for the
	// binary. Empty when StateNotInstalled or when the version
	// probe failed. Used by the admin UI to compare against
	// LatestRelease and offer upgrade.
	Version string `json:"version,omitempty"`

	// DownloadURL is the GitHub releases page operators land on
	// when StateNotInstalled. Always set so the UI can render a
	// "Get ycode" link regardless of state (the operator may want
	// to upgrade an installed version).
	DownloadURL string `json:"download_url"`

	// PlatformSupported reports whether ycode publishes a binary for
	// this OS+arch. Set to false on darwin/amd64 and windows/amd64
	// today — the release workflow excludes them pending podman
	// integration work. UI renders "ycode is not currently available
	// for <platform>" instead of an install link.
	PlatformSupported bool `json:"platform_supported"`
}

const (
	// DownloadURL is the GitHub releases page link the admin UI shows
	// when ycode isn't installed. Stable across releases — links to
	// /releases (not /releases/latest) so users see the changelog
	// before downloading.
	DownloadURL = "https://github.com/qiangli/ycode/releases"

	// httpProbeTimeout caps how long we'll wait for the api endpoint
	// to answer. ycode answers from local sqlite — should be sub-
	// millisecond — anything beyond 500 ms is a sick process.
	httpProbeTimeout = 500 * time.Millisecond

	// versionProbeTimeout caps how long we'll wait for `ycode
	// version` to return. Spawning a Go binary cold takes ~50 ms;
	// 2 s leaves headroom for slow disks.
	versionProbeTimeout = 2 * time.Second
)

// Detect runs all the lookups and returns a single Info snapshot.
// Safe to call concurrently; no state is mutated. Caller should
// cache via BuiltinDetector-style TTL — this is not free (1-2
// network probes plus a process spawn for version).
func Detect() Info {
	info := Info{
		ManifestPath:      defaultManifestPath(),
		DownloadURL:       DownloadURL,
		PlatformSupported: platformSupported(),
	}

	// Step 1: try to read the manifest. If present, probe its
	// endpoint to confirm there's a live process. This precedence
	// is right because a running ycode is the authoritative signal
	// — the binary on PATH doesn't matter if the running process
	// answers.
	if endpoint, ok := readManifestAPI(info.ManifestPath); ok {
		info.APIEndpoint = endpoint
		if httpAlive(endpoint) {
			info.State = StateRunning
			info.BinaryPath = locateBinary()
			info.Version = probeVersion(info.BinaryPath)
			return info
		}
		// Manifest exists but endpoint is dead — process crashed,
		// rebooted, or never cleaned up after stop. The operator can
		// remove the file (or rerun `ycode serve`, which rewrites it).
		info.State = StateStaleManifest
	}

	// Step 2: no live process. Is there an installable binary?
	if bin := locateBinary(); bin != "" {
		info.BinaryPath = bin
		info.Version = probeVersion(bin)
		if info.State == "" {
			info.State = StateInstalled
		}
		return info
	}

	info.State = StateNotInstalled
	return info
}

// defaultManifestPath returns the canonical `~/.agents/ycode/manifest.json`
// for the current OS user. Trailing slash discipline matches ycode's
// own writers exactly so the comparison is bit-stable.
func defaultManifestPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".agents", "ycode", "manifest.json")
}

// readManifestAPI parses the manifest file at path and returns the
// api endpoint URL when it's present + non-empty. ok=false when the
// file is missing, unreadable, malformed, or has no api endpoint —
// any of those collapse into "no usable manifest."
func readManifestAPI(path string) (endpoint string, ok bool) {
	if path == "" {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var m struct {
		Endpoints map[string]string `json:"endpoints"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", false
	}
	api := strings.TrimSpace(m.Endpoints["api"])
	if api == "" {
		return "", false
	}
	return api, true
}

// httpAlive probes the given URL with a short timeout. Any HTTP
// response (2xx/4xx/5xx) confirms a daemon — only dial / connect
// errors count as "dead." Matches builtin_apps.go's probeHTTP shape
// so the two stay consistent.
func httpAlive(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), httpProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: httpProbeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode > 0
}

// locateBinary tries PATH first, then $HOME/bin/ycode as a fallback
// — the convention outpost itself uses for its own install. Returns
// "" when neither finds it. Windows callers need the .exe extension
// stripped from PATH lookups, which exec.LookPath handles natively.
func locateBinary() string {
	if p, err := exec.LookPath("ycode"); err == nil {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	candidates := []string{
		filepath.Join(home, "bin", "ycode"),
		filepath.Join(home, ".local", "bin", "ycode"),
	}
	if runtime.GOOS == "windows" {
		for i, p := range candidates {
			candidates[i] = p + ".exe"
		}
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// probeVersion shells out to `ycode version --short`. Returns ""
// when the binary doesn't accept --short (older releases), can't
// exec, or times out. Format is whatever the binary prints — we
// don't impose semver shape, just trim whitespace.
func probeVersion(bin string) string {
	if bin == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version", "--short").Output()
	if err != nil {
		// Fall back to `ycode --version` for older ycode builds that
		// don't have the dedicated subcommand.
		ctx2, cancel2 := context.WithTimeout(context.Background(), versionProbeTimeout)
		defer cancel2()
		out, err = exec.CommandContext(ctx2, bin, "--version").Output()
		if err != nil {
			return ""
		}
	}
	return strings.TrimSpace(string(out))
}

// platformSupported reports whether the ycode release pipeline
// currently ships a binary for this OS+arch combination. Mirrors the
// matrix in ycode's .github/workflows/release.yml — keep in sync.
func platformSupported() bool {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64", "linux/arm64", "darwin/arm64":
		return true
	default:
		// darwin/amd64 and windows/amd64 are blocked by podman
		// integration in ycode; they need code work upstream
		// before binaries can be cut. Outpost UI shows "ycode is
		// not available for this platform" in this case.
		return false
	}
}

// ReleaseAssetName is the file name pattern ycode publishes per
// platform on GitHub Releases. Used by future install code; returns
// "" for unsupported platforms.
func ReleaseAssetName() string {
	if !platformSupported() {
		return ""
	}
	ext := ".tar.gz"
	return fmt.Sprintf("ycode-%s-%s%s", runtime.GOOS, runtime.GOARCH, ext)
}
