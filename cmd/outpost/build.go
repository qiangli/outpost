package main

// outpost build — self-build from source, using outpost's own embedded
// git client (go-git) for every repository operation. The only external
// prerequisite is the Go toolchain; no system git, no bash, no make.
// This is the scripted form of the self-rebuild flow:
//
//	outpost git clone https://github.com/qiangli/outpost.git
//	outpost shell ./scripts/build.sh
//
// Steps: clone --repo at --ref (or reuse --src), then `go build` with the
// commit + dirty flag stamped so `outpost version` stays traceable to a SHA.
// Siblings are go.mod pins, so the go command fetches them; no clones.
//
// The command deliberately stops at producing a binary — swapping the
// running install is `outpost upgrade --local <built>`, which keeps the
// .previous rollback contract in one place.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	outgit "github.com/qiangli/yoke/git"
)

func buildCmd() *cobra.Command {
	var (
		repoFlag string
		refFlag  string
		srcFlag  string
		dirFlag  string
		outFlag  string
	)
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build outpost from source (embedded git; only the Go toolchain required)",
		Long: `outpost build compiles outpost from source. Repository operations use
outpost's embedded git client, so the only prerequisite is Go 1.25+ on
PATH (https://go.dev/dl/ or 'winget install GoLang.Go').

By default it clones the upstream repo at main into a work directory and
builds with the commit stamped into the binary; the go command fetches the
sibling modules go.mod pins.

To replace the running install afterwards, use the produced path with
'outpost upgrade --local <path>' — that keeps the .previous rollback.`,
		Example: `  outpost build                                  # main from GitHub → <cache>/outpost/build/outpost/bin/
  outpost build --ref v0.3.0                     # a tag
  outpost build --ref 7fc8d10                    # any commit
  outpost build --src .                          # an existing checkout (skips clone)
  outpost build -o ./outpost-new && outpost upgrade --local ./outpost-new`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBuild(cmd, repoFlag, refFlag, srcFlag, dirFlag, outFlag)
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "https://github.com/qiangli/outpost.git", "Source repository URL")
	cmd.Flags().StringVar(&refFlag, "ref", "", "Branch, tag, or commit SHA to build (default: remote HEAD, i.e. main)")
	cmd.Flags().StringVar(&srcFlag, "src", "", "Existing source checkout to build as-is (skips the clone; --repo/--ref ignored)")
	cmd.Flags().StringVar(&dirFlag, "dir", "", "Work directory for the clone (default: <user-cache>/outpost/build)")
	cmd.Flags().StringVarP(&outFlag, "output", "o", "", "Output binary path (default: <src>/bin/outpost[.exe])")
	return cmd
}

func runBuild(cmd *cobra.Command, repo, ref, src, dir, out string) error {
	stdout := cmd.OutOrStdout()

	goBin, err := exec.LookPath("go")
	if err != nil {
		return errors.New("Go toolchain not found on PATH — install Go 1.25+ from https://go.dev/dl/ (Windows: winget install GoLang.Go)")
	}

	// ---- 1. source tree: reuse --src, or clone --repo at --ref ----
	if src == "" {
		if dir == "" {
			cacheDir, err := os.UserCacheDir()
			if err != nil {
				return fmt.Errorf("resolve cache dir (pass --dir): %w", err)
			}
			dir = filepath.Join(cacheDir, "outpost", "build")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		src = filepath.Join(dir, "outpost")
		if _, err := os.Stat(filepath.Join(src, ".git")); err == nil {
			return fmt.Errorf("%s already exists — build it as-is with --src %s, or remove it for a fresh clone", src, src)
		}
		fmt.Fprintf(stdout, "==> cloning %s%s -> %s\n", repo, refSuffix(ref), src)
		if err := cloneAtRef(repo, src, ref); err != nil {
			return err
		}
	} else {
		var err error
		if src, err = filepath.Abs(src); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(src, "go.mod")); err != nil {
			return fmt.Errorf("--src %s does not look like an outpost checkout (no go.mod)", src)
		}
	}

	// ---- 2. go build with provenance ldflags ----
	commit, dirty := "", "false"
	if rp, err := outgit.RevParse(outgit.RevParseOptions{RepoPath: src, Short: 7}); err == nil {
		commit = rp.Short
		if rp.Dirty {
			dirty = "true"
		}
	}
	ld := fmt.Sprintf("-X github.com/qiangli/outpost/internal/agent.ldCommit=%s -X github.com/qiangli/outpost/internal/agent.ldDirty=%s", commit, dirty)

	targetGOOS := os.Getenv("GOOS")
	if targetGOOS == "" {
		targetGOOS = runtime.GOOS
	}
	if out == "" {
		out = filepath.Join(src, "bin", "outpost")
		if targetGOOS == "windows" {
			out += ".exe"
		}
	} else if out, err = filepath.Abs(out); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "==> go build (commit %s, dirty=%s)\n", commit, dirty)
	gc := exec.CommandContext(cmd.Context(), goBin, "build", "-trimpath", "-ldflags", ld, "-o", out, "./cmd/outpost")
	gc.Dir = src
	gc.Stdout = stdout
	gc.Stderr = cmd.ErrOrStderr()
	gc.Env = os.Environ()
	if os.Getenv("CGO_ENABLED") == "" {
		// No cgo deps; disabling is what makes cross-compile work everywhere.
		gc.Env = append(gc.Env, "CGO_ENABLED=0")
	}
	if err := gc.Run(); err != nil {
		return fmt.Errorf("go build failed: %w", err)
	}
	fmt.Fprintf(stdout, "[ok] built %s\n", out)
	fmt.Fprintf(stdout, "     to replace the current install: outpost upgrade --local %s\n", out)
	return nil
}

// cloneAtRef clones url into path. A branch or tag ref rides the clone
// itself; anything else (a commit SHA) falls back to a full clone of
// the default branch followed by a detached checkout.
func cloneAtRef(url, path, ref string) error {
	if ref != "" {
		if _, err := outgit.Clone(outgit.CloneOptions{URL: url, Path: path, Branch: ref, SingleBranch: true}); err == nil {
			return nil
		}
		// Branch/tag lookup failed — retry as default-branch clone +
		// revision checkout. Clean up the partial clone first.
		_ = os.RemoveAll(path)
		if _, err := outgit.Clone(outgit.CloneOptions{URL: url, Path: path}); err != nil {
			return fmt.Errorf("clone %s: %w", url, err)
		}
		if _, err := outgit.Checkout(outgit.CheckoutOptions{RepoPath: path, Branch: ref}); err != nil {
			return fmt.Errorf("checkout %s: %w", ref, err)
		}
		return nil
	}
	if _, err := outgit.Clone(outgit.CloneOptions{URL: url, Path: path}); err != nil {
		return fmt.Errorf("clone %s: %w", url, err)
	}
	return nil
}

func refSuffix(ref string) string {
	if ref == "" {
		return ""
	}
	return " @ " + ref
}
