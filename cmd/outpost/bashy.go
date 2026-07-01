package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/binmgr"
)

const defaultBashyRepo = "qiangli/bashy"

// outpost bashy is the bootstrap bridge for machines that have outpost but not
// bashy yet. Outpost stays the small production seed: it resolves the released
// bashy archive, verifies it through the release checksums, extracts/caches the
// binary, and optionally copies it into a caller-chosen install path. Once a
// bashy binary exists, bashy owns source checkout/build/update.
func bashyCmd() *cobra.Command {
	var (
		version    string
		repo       string
		install    string
		installDir string
	)
	cmd := &cobra.Command{
		Use:   "bashy",
		Short: "Download, verify, and cache the bashy system shell",
		Long: `outpost bashy seeds bashy onto a machine that already has outpost.

It resolves a qiangli/bashy GitHub release for this OS/arch, downloads and
verifies the archive using the release checksums, extracts bashy, and prints the
cached path. Pass --install PATH or --install-dir DIR to copy it into a stable
location after verification.

This command intentionally does not use system git, system bash, or system
coreutils. Once bashy exists, use bashy git / bashy dag / bashy self for the
rest of the build and update workflow.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if install != "" && installDir != "" {
				return errors.New("--install and --install-dir are mutually exclusive")
			}
			path, err := ensureBashy(cmd.Context(), bashyResolveOptions{
				Repo:    repo,
				Version: version,
			})
			if err != nil {
				return err
			}
			target := ""
			switch {
			case install != "":
				target, err = filepath.Abs(install)
				if err != nil {
					return err
				}
			case installDir != "":
				target, err = bashyInstallTarget(installDir)
				if err != nil {
					return err
				}
			}
			if target != "" {
				if err := installBashyExecutable(path, target); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\n", target)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", path)
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "latest", "bashy release tag to fetch")
	cmd.Flags().StringVar(&repo, "repo", defaultBashyRepo, "GitHub owner/name for bashy releases")
	cmd.Flags().StringVar(&install, "install", "", "Install bashy to this exact executable path after caching")
	cmd.Flags().StringVar(&installDir, "install-dir", "", "Install bashy as bashy[.exe] in this directory after caching")
	return cmd
}

type bashyResolveOptions struct {
	Repo    string
	Version string
}

func ensureBashy(ctx context.Context, opts bashyResolveOptions) (string, error) {
	tool, err := resolveBashyTool(ctx, opts)
	if err != nil {
		return "", err
	}
	return binmgr.Ensure(ctx, tool)
}

func resolveBashyTool(ctx context.Context, opts bashyResolveOptions) (binmgr.Tool, error) {
	repo := strings.TrimSpace(opts.Repo)
	if repo == "" {
		repo = defaultBashyRepo
	}
	version := strings.TrimSpace(opts.Version)
	if version == "" {
		version = "latest"
	}
	return binmgr.ResolveGitHub(ctx, binmgr.GitHubSpec{
		Name:       "bashy",
		Repo:       repo,
		Version:    version,
		Member:     bashyArchiveMember(),
		AssetMatch: matchBashyReleaseAsset,
	})
}

func bashyArchiveMember() string {
	if runtime.GOOS == "windows" {
		return "bashy.exe"
	}
	return "bashy"
}

func matchBashyReleaseAsset(name, goos, goarch string) bool {
	n := strings.ToLower(name)
	if !strings.HasPrefix(n, "bashy-") {
		return false
	}
	if !strings.Contains(n, strings.ToLower(goos)) {
		return false
	}
	return strings.Contains(n, strings.ToLower(goarch))
}

func bashyInstallTarget(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("install directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(abs, bashyArchiveMember()), nil
}

func installBashyExecutable(src, dst string) error {
	if src == "" || dst == "" {
		return errors.New("source and destination are required")
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	removeTmp = false
	return nil
}
