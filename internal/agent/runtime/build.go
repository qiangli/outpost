package runtime

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// Only runtime scripts/manifests are embedded; the legacy Dockerfile is unused.
//
//go:embed image/*.sh image/*.yaml
var imageFS embed.FS

// BuildOptions controls `outpost cluster build-runtime`. All fields
// are optional — sensible defaults match the supervisor in Up().
type BuildOptions struct {
	// Resolver supplies signed, pinned Linux artifacts. Nil uses yoke binmgr.
	Resolver BinaryResolver
	// SourceDir is an outpost checkout used to cross-build outpost and its CNI.
	// Empty uses the current directory. No third-party artifact downloads occur.
	SourceDir string

	// Tag is the image reference to produce (e.g.
	// "outpost-runtime:dev" or a registry-qualified name for push).
	// Empty defaults to DefaultImage.
	Tag string

	// TargetArch is the linux architecture of the runtime image
	// ("amd64" or "arm64"). Empty defaults to the host's arch — the
	// usual case, since the container runs on the same machine the
	// outpost daemon does. Override only when cross-building (e.g.
	// from an arm64 dev machine for an amd64 production host).
	TargetArch string

	// PodmanBin overrides the autodetected `podman`/`docker` binary
	// used to drive the build. Empty triggers the same PATH lookup
	// the supervisor uses (pickPodmanBin).
	PodmanBin string

	// Stdout / Stderr receive the podman build's output. Defaults
	// (when nil) route to os.Stdout / os.Stderr so the operator
	// sees the build progress interactively.
	Stdout, Stderr *os.File
}

// BuildImage assembles a scratch rootfs from verified binaries, then asks the
// local engine to package it. There are no RUN steps or base-image pulls.
func BuildImage(ctx context.Context, opts BuildOptions) (string, error) {
	tag := opts.Tag
	if tag == "" {
		tag = DefaultImage
	}
	arch := opts.TargetArch
	if arch == "" {
		arch = hostLinuxArch()
	}
	if arch != "amd64" && arch != "arm64" {
		return "", fmt.Errorf("build-runtime: unsupported target arch %q (want amd64 or arm64)", arch)
	}

	ctxDir, err := assembleImageContext(ctx, opts, arch)
	if err != nil {
		return "", fmt.Errorf("build-runtime: assemble context: %w", err)
	}
	keepCtx := os.Getenv("OUTPOST_BUILDRUNTIME_KEEP_CTX") != ""
	defer func() {
		if keepCtx {
			slog.Info("build-runtime: keeping context dir for inspection", "dir", ctxDir)
			return
		}
		if rmErr := os.RemoveAll(ctxDir); rmErr != nil {
			slog.Warn("build-runtime: tempdir cleanup", "dir", ctxDir, "err", rmErr)
		}
	}()

	bin, err := pickPodmanBin(opts.PodmanBin)
	if err != nil {
		return "", err
	}
	args := []string{"build", "-t", tag, ctxDir}

	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	slog.Info("build-runtime: starting podman build",
		"bin", bin, "tag", tag, "arch", arch, "context", ctxDir)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build-runtime: %s build: %w", filepath.Base(bin), err)
	}

	// Reclaim the previous image's now-untagged layers. Each rebuild
	// retags outpost-runtime:dev onto fresh layers and orphans the old
	// ones; on the small (20 GB) podman VM those accumulate until the
	// kubelet trips DiskPressure and evicts pods. Prune only DANGLING
	// (untagged, unreferenced) images — never -a — so the running
	// container's image and any tagged images are untouched. Best-effort:
	// a prune failure must not fail the build.
	prune := exec.CommandContext(ctx, bin, "image", "prune", "-f")
	prune.Stdout = stderr // narrate on the same stream as the build
	prune.Stderr = stderr
	if perr := prune.Run(); perr != nil {
		slog.Warn("build-runtime: dangling-image prune failed (non-fatal)", "err", perr)
	}
	return tag, nil
}

// EnsureImage builds the runtime image iff it is absent OR was built by a
// different outpost version — so an `outpost upgrade` (or any binary swap)
// makes the container image self-rebuild on the next DKS agent start,
// with no manual `outpost cluster build-runtime` step. It returns whether
// a rebuild happened.
//
// Why a host marker file instead of an image label: the pinned podman
// engine's `build` accepts only --build-arg/-f/-t (no --label), so the
// image can't carry the version itself. `<stateDir>/runtime-image-version`
// records the version that produced the current image; a mismatch (or a
// missing image) triggers a rebuild. Cheap: one `image inspect` in the
// steady state, no rebuild.
func EnsureImage(ctx context.Context, opts BuildOptions, wantVersion, stateDir string) (bool, error) {
	tag := opts.Tag
	if tag == "" {
		tag = DefaultImage
	}
	bin, err := pickPodmanBin(opts.PodmanBin)
	if err != nil {
		return false, err
	}
	marker := filepath.Join(stateDir, "runtime-image-version")
	have, _ := os.ReadFile(marker)
	imagePresent := exec.CommandContext(ctx, bin, "image", "inspect", tag).Run() == nil
	if imagePresent && strings.TrimSpace(string(have)) == strings.TrimSpace(wantVersion) && wantVersion != "" {
		return false, nil // up to date
	}
	slog.Info("build-runtime: (re)building runtime image",
		"tag", tag, "want_version", wantVersion, "image_present", imagePresent)
	if _, err := BuildImage(ctx, opts); err != nil {
		return false, err
	}
	if err := os.MkdirAll(stateDir, 0o700); err == nil {
		_ = os.WriteFile(marker, []byte(strings.TrimSpace(wantVersion)+"\n"), 0o644)
	}
	return true, nil
}

// hostLinuxArch picks the linux architecture matching the host's
// runtime. Container is always linux/* regardless of host OS, so we
// only care about the arch dimension.
func hostLinuxArch() string {
	switch goruntime.GOARCH {
	case "amd64", "arm64":
		return goruntime.GOARCH
	default:
		// Less common host archs (e.g. 386, s390x) — the build
		// fails above with a clear "unsupported arch" message
		// rather than producing an image the daemon can't run.
		return ""
	}
}
