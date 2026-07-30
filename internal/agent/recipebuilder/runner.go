package recipebuilder

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Runner performs the three side-effecting steps of building a recipe. It is an
// interface so the poll/build orchestration is unit-testable without a real
// podman/git — the daemon uses execRunner, tests use a fake.
type Runner interface {
	// Clone resolves a git build context (repo@ref) into dest.
	Clone(ctx context.Context, repo, ref, dest string) error
	// Build builds contextDir's dockerfile natively into ref.
	Build(ctx context.Context, platform, dockerfile, ref, contextDir string) error
	// Load streams the built image into the node's k3s containerd (via the
	// <name>-runtime container's `k3s ctr images import`).
	Load(ctx context.Context, ref, runtimeContainer string) error
	// ImagePresent checks the image store of the current runtime container, not
	// the host build cache. Runtime recreation can erase containerd while the
	// recipe digest remains unchanged.
	ImagePresent(ctx context.Context, ref, runtimeContainer string) (bool, error)
}

// execRunner shells out to `bashy podman` / `bashy git`. bashyBin is the
// resolved bashy executable (the daemon threads it in — see cmd/outpost/bashy.go).
type execRunner struct {
	bashyBin string
	stderr   io.Writer // build/clone diagnostics; nil → os.Stderr
}

func (e execRunner) errw() io.Writer {
	if e.stderr != nil {
		return e.stderr
	}
	return os.Stderr
}

func (e execRunner) Clone(ctx context.Context, repo, ref, dest string) error {
	if err := e.run(ctx, e.bashyBin, "git", "clone", "--depth", "1", repo, dest); err != nil {
		return fmt.Errorf("clone %s: %w", repo, err)
	}
	if ref == "" {
		return nil
	}
	// A shallow clone can't check out an arbitrary ref; fetch it first.
	if err := e.run(ctx, e.bashyBin, "git", "-C", dest, "fetch", "--depth", "1", "origin", ref); err != nil {
		return fmt.Errorf("fetch %s: %w", ref, err)
	}
	if err := e.run(ctx, e.bashyBin, "git", "-C", dest, "checkout", ref); err != nil {
		return fmt.Errorf("checkout %s: %w", ref, err)
	}
	return nil
}

func (e execRunner) Build(ctx context.Context, platform, dockerfile, ref, contextDir string) error {
	return e.run(ctx, e.bashyBin, "podman", "build",
		"--platform", platform, "-f", dockerfile, "-t", ref, contextDir)
}

// Load pipes `bashy podman save <ref>` into
// `bashy podman exec -i <runtime> k3s ctr images import -`.
func (e execRunner) Load(ctx context.Context, ref, runtimeContainer string) error {
	save := exec.CommandContext(ctx, e.bashyBin, "podman", "save", ref)
	imp := exec.CommandContext(ctx, e.bashyBin, "podman", "exec", "-i", runtimeContainer,
		"k3s", "ctr", "images", "import", "-")

	pipe, err := save.StdoutPipe()
	if err != nil {
		return err
	}
	imp.Stdin = pipe
	save.Stderr = e.errw()
	imp.Stderr = e.errw()

	if err := imp.Start(); err != nil {
		return fmt.Errorf("start import: %w", err)
	}
	if err := save.Run(); err != nil {
		_ = imp.Wait()
		return fmt.Errorf("save %s: %w", ref, err)
	}
	if err := imp.Wait(); err != nil {
		return fmt.Errorf("import %s into %s: %w", ref, runtimeContainer, err)
	}
	return nil
}

func (e execRunner) ImagePresent(ctx context.Context, ref, runtimeContainer string) (bool, error) {
	cmd := exec.CommandContext(ctx, e.bashyBin, "podman", "exec", runtimeContainer,
		"k3s", "ctr", "images", "ls", "-q", "name=="+ref)
	cmd.Stderr = e.errw()
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("inspect %s in %s: %w", ref, runtimeContainer, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == ref {
			return true, nil
		}
	}
	return false, nil
}

func (e execRunner) run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = e.errw()
	cmd.Stdout = e.errw()
	return cmd.Run()
}
