package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/binmgr"
)

const defaultBashyRepo = "qiangli/bashy"

// bashyAutoInstallBackoff throttles the self-heal download so an offline or
// rate-limited host doesn't re-hit GitHub on every supervisor tick.
const bashyAutoInstallBackoff = 5 * time.Minute

// bashyBinaryResolver locates the bashy executable used to run
// outpost-supervised services (`bashy <svc> start|status|stop`), self-healing
// when it is missing. Resolution order: an explicit $OUTPOST_BASHY_BIN, then
// PATH, then the outpost binary's own dir and the common install locations
// (a daemon's PATH is narrow — launchd/systemd strip ~/bin), and finally —
// when bashy is genuinely absent — a download+verify+cache of the latest
// release via binmgr (the same path `outpost bashy` uses). The resolved path
// is cached; a tick that can't resolve (e.g. offline) returns an error and the
// supervisor's 30s loop retries, so a service recovers as soon as bashy is
// installed manually or the network returns. Simply failing forever is not an
// option — a missing userland should self-remediate.
type bashyBinaryResolver struct {
	mu        sync.Mutex
	cached    string
	lastFetch time.Time
}

// bashyResolver is the process-wide resolver shared by every supervised service.
var bashyResolver = &bashyBinaryResolver{}

// Path returns an absolute path to a runnable bashy, provisioning it if needed.
func (r *bashyBinaryResolver) Path(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Fast path: a previously resolved binary that is still runnable.
	if r.cached != "" {
		if isExecutableFile(r.cached) {
			return r.cached, nil
		}
		r.cached = "" // vanished (upgraded out from under us) — re-resolve.
	}

	// 1. Operator override.
	if p := strings.TrimSpace(os.Getenv("OUTPOST_BASHY_BIN")); p != "" && isExecutableFile(p) {
		r.cached = p
		return p, nil
	}
	// 2. PATH.
	if p, err := exec.LookPath("bashy"); err == nil {
		r.cached = p
		return p, nil
	}
	// 3. Common install locations the daemon PATH may not include.
	for _, cand := range bashyCandidatePaths() {
		if isExecutableFile(cand) {
			r.cached = cand
			return cand, nil
		}
	}
	// 4. Self-heal: fetch the latest release (throttled).
	if !r.lastFetch.IsZero() && time.Since(r.lastFetch) < bashyAutoInstallBackoff {
		return "", fmt.Errorf("bashy not found; auto-install backing off (retry in %s)",
			(bashyAutoInstallBackoff - time.Since(r.lastFetch)).Round(time.Second))
	}
	r.lastFetch = time.Now()
	slog.Info("bashy not found on PATH or common locations; fetching latest release to self-heal")
	p, err := ensureBashy(ctx, bashyResolveOptions{})
	if err != nil {
		return "", fmt.Errorf("bashy auto-install failed (will retry): %w", err)
	}
	r.cached = p
	slog.Info("bashy auto-installed", "path", p)
	return p, nil
}

// bashyCandidatePaths lists the usual bashy install locations, checked when it
// is not on the daemon's (often narrow) PATH.
func bashyCandidatePaths() []string {
	name := bashyArchiveMember() // bashy or bashy.exe
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe)) // installed alongside outpost
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "bin"), filepath.Join(home, ".local", "bin"))
	}
	dirs = append(dirs, "/usr/local/bin", "/opt/homebrew/bin")
	if cache, err := os.UserCacheDir(); err == nil {
		dirs = append(dirs, filepath.Join(cache, "outpost", "bin"))
	}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if strings.TrimSpace(d) != "" {
			out = append(out, filepath.Join(d, name))
		}
	}
	return out
}

// isExecutableFile reports whether path is a runnable file (regular file, and
// on unix carrying an execute bit).
func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true // executability is by extension, not a mode bit
	}
	return fi.Mode().Perm()&0o111 != 0
}

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
