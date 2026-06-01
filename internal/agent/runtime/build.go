package runtime

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// imageFS is the build context for outpost cluster build-runtime. The
// `all:` prefix matters: Go's default embed walker skips directories
// whose name begins with "_" or "." and files in them; we don't have
// any of those today, but `all:` keeps the pattern robust if a future
// scaffolding tool drops an `.env` or `_tmp/` next to the Dockerfile.
//
//go:embed all:image
var imageFS embed.FS

// imageFSRoot is the top-level directory inside imageFS. Kept as a
// constant so callers that need to walk the FS don't hardcode the
// string and drift from the embed pattern.
const imageFSRoot = "image"

// BuildOptions controls `outpost cluster build-runtime`. All fields
// are optional — sensible defaults match the supervisor in Up().
type BuildOptions struct {
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

// BuildImage materializes the embedded build context (Dockerfile +
// entrypoint.sh + cni/) to a tempdir and runs `podman build` against
// it. Idempotent: every invocation is a fresh tempdir, but podman's
// own layer cache means a second run with no source changes returns
// in seconds (each RUN line hashes its inputs; identical inputs reuse
// the cached layer).
//
// Returns the image tag that was produced and any build error. The
// tempdir is cleaned up on return regardless of outcome.
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

	bin, err := pickPodmanBin(opts.PodmanBin)
	if err != nil {
		return "", err
	}

	ctxDir, err := materializeImageFS()
	if err != nil {
		return "", fmt.Errorf("build-runtime: materialize embed: %w", err)
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

	// ycode podman (our pinned engine — see ~/.claude memory:
	// feedback_ycode_podman_only.md) accepts only --build-arg, -f,
	// and -t on its build subcommand. Cross-arch is handled via the
	// TARGETARCH build-arg the Dockerfile reads in its k3s/frp/cni
	// RUN lines, so we don't need --platform here. Operators who
	// want a clean rebuild can `podman image rm outpost-runtime:dev`
	// before running build-runtime — no --no-cache exposed (caching
	// is the whole point: public-registry rate limits make rebuilds
	// expensive otherwise).
	args := []string{
		"build",
		"--build-arg", "TARGETARCH=" + arch,
		"-t", tag,
		ctxDir,
	}

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
	return tag, nil
}

// materializeImageFS writes the embedded build context to a fresh
// tempdir and returns its path. The directory layout matches what's
// embedded — Dockerfile + entrypoint.sh at the root, cni/ subtree
// for the multi-stage builder.
func materializeImageFS() (string, error) {
	tmp, err := os.MkdirTemp("", "outpost-runtime-build-")
	if err != nil {
		return "", err
	}
	walkErr := fs.WalkDir(imageFS, imageFSRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, imageFSRoot+"/")
		if rel == imageFSRoot { // root itself
			return nil
		}
		dst := filepath.Join(tmp, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := imageFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read embed %s: %w", path, err)
		}
		// The embedded entrypoint.sh needs to be executable — Go's
		// embed.FS doesn't preserve file modes, so we set the bit
		// ourselves for any .sh file we drop. Source files stay
		// 0644; podman build doesn't care about input perms beyond
		// the COPYs that re-chmod inside the Dockerfile.
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0o755
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir for %s: %w", dst, err)
		}
		return os.WriteFile(dst, data, mode)
	})
	if walkErr != nil {
		_ = os.RemoveAll(tmp)
		return "", walkErr
	}
	// Generate cni/go.mod at materialize time rather than committing
	// it to the source tree. Go's embed walker treats a directory
	// with its own go.mod as a separate (nested) module and silently
	// excludes its contents from patterns rooted in the outer
	// module — so shipping the file in tree would defeat the embed.
	// The module name matches the outer-module path of the cni
	// directory, so the imports written in cni/main.go (which are
	// valid from the outer module's perspective too, so `go build
	// ./...` still type-checks them) resolve locally in the
	// container.
	goMod := []byte("module github.com/qiangli/outpost/internal/agent/runtime/image/cni\n\ngo 1.25\n")
	if err := os.WriteFile(filepath.Join(tmp, "cni", "go.mod"), goMod, 0o644); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("write cni/go.mod: %w", err)
	}
	return tmp, nil
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
