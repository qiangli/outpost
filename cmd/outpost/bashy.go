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

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/bashypath"
	"github.com/qiangli/yoke/pkg/binmgr"
)

const defaultBashyRepo = "qiangli/bashy"

// DefaultBashyVersion is used only by untagged development builds. Releases
// derive the paired bashy tag from their own build stamp, including channel.
const DefaultBashyVersion = "latest"

// bashyAutoInstallBackoff throttles the self-heal download so an offline or
// rate-limited host doesn't re-hit GitHub on every supervisor tick.
const bashyAutoInstallBackoff = 5 * time.Minute

// bashyBinaryResolver locates the bashy executable used to run
// outpost-supervised services (`bashy <svc> start|status|stop`), self-healing
// when it is missing. Resolution order: executable sibling, then
// $OUTPOST_BASHY_BIN, PATH, and the common install locations
// (a daemon's PATH is narrow — launchd/systemd strip ~/bin), and finally —
// when bashy is genuinely absent — a download+verify+cache of the latest
// release via binmgr (the same path `outpost bashy` uses). The resolved path
// is cached; a tick that can't resolve (e.g. offline) returns an error and the
// supervisor's 30s loop retries, so a service recovers as soon as bashy is
// installed manually or the network returns. Simply failing forever is not an
// option — a missing userland should self-remediate.
type bashyBinaryResolver struct {
	mu         sync.Mutex
	cached     string
	lastFetch  time.Time
	reconciled bool // version-reconcile runs once per boot (per outpost lifetime)
	// version pins the release the self-heal auto-install fetches ("" or
	// "latest" = newest). It governs ONLY the auto-install when bashy is
	// absent — an already-installed bashy on PATH is used as-is (the resolver
	// self-heals a missing userland; it does not enforce a version over an
	// operator's existing install). Pin it in production so an outpost restart
	// can't silently pull a new bashy.
	version string
	// executable is a test seam; production resolves os.Executable.
	executable func() (string, error)
}

// SetVersion pins the bashy release the self-heal auto-install fetches. Called
// once at boot from the resolved FileConfig.
func (r *bashyBinaryResolver) SetVersion(v string) {
	r.mu.Lock()
	r.version = strings.TrimSpace(v)
	r.mu.Unlock()
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

	// Find an existing bashy. `override` (OUTPOST_BASHY_BIN) and a plain PATH hit
	// are treated as operator-owned and never auto-rolled; an outpost-managed
	// install (outpost-adjacent / cache) is reconciled to the matched version.
	found, managed, localErr := bashypath.Find(r.executable)
	if localErr != nil && !errors.Is(localErr, bashypath.ErrNotFound) {
		return "", localErr
	}
	if found != "" {
		// Reconcile the outpost-managed bashy to the matched version, ONCE per
		// boot. This is the fleet auto-roll: after outpost upgrades + restarts,
		// its first supervised-service start pulls the matching bashy.
		if managed && !r.reconciled {
			r.reconciled = true
			if np := r.reconcile(ctx, found); np != "" {
				found = np
			}
		}
		r.cached = found
		return found, nil
	}

	// 4. Self-heal: bashy is genuinely absent — install the MATCHED version
	//    (the outpost-carried default, or an explicit pin), throttled.
	if !r.lastFetch.IsZero() && time.Since(r.lastFetch) < bashyAutoInstallBackoff {
		return "", fmt.Errorf("bashy not found; auto-install backing off (retry in %s)",
			(bashyAutoInstallBackoff - time.Since(r.lastFetch)).Round(time.Second))
	}
	r.lastFetch = time.Now()
	version := r.effectiveVersion()
	slog.Info("bashy not found on PATH or common locations; fetching to self-heal", "version", version)
	p, err := ensureBashy(ctx, bashyResolveOptions{Version: version})
	if err != nil {
		return "", fmt.Errorf("bashy auto-install failed (will retry): %w", err)
	}
	r.cached = p
	slog.Info("bashy auto-installed", "path", p, "version", version)
	return p, nil
}

// effectiveVersion is the bashy release the resolver installs / reconciles to:
// an explicit operator pin (a specific tag) wins; otherwise the outpost-carried
// DefaultBashyVersion (the matched pair). An empty/"latest" pin means "track the
// outpost default" so bashy stays locked to outpost.
func (r *bashyBinaryResolver) effectiveVersion() string {
	v := strings.TrimSpace(r.version)
	if v != "" && !strings.EqualFold(v, "latest") {
		return v
	}
	return pairedBashyVersion(agent.ReadBuildInfo().Version)
}

func pairedBashyVersion(stamp string) string {
	stamp = strings.TrimSpace(stamp)
	if stamp == "" || stamp == "dev" || stamp == "(devel)" {
		return DefaultBashyVersion
	}
	return "v" + strings.TrimPrefix(stamp, "v")
}

// reconcile reinstalls bashy at path to effectiveVersion when the installed
// version differs (the fleet auto-roll). Returns the path on a successful
// re-install, "" otherwise (unknown version, already matched, or a soft failure
// that keeps the current binary). Throttled by the same backoff as self-heal.
func (r *bashyBinaryResolver) reconcile(ctx context.Context, path string) string {
	want := r.effectiveVersion()
	if want == "" || strings.EqualFold(want, "latest") {
		return ""
	}
	have := bashyInstalledVersion(ctx, path)
	if have == "" || sameBashyVersion(have, want) {
		return "" // couldn't read it, or already matched — leave it alone
	}
	if !r.lastFetch.IsZero() && time.Since(r.lastFetch) < bashyAutoInstallBackoff {
		return ""
	}
	r.lastFetch = time.Now()
	slog.Info("bashy version mismatch with outpost; rolling to match", "have", have, "want", want, "path", path)
	cached, err := ensureBashy(ctx, bashyResolveOptions{Version: want})
	if err != nil {
		slog.Warn("bashy auto-roll: fetch failed (keeping current bashy)", "err", err)
		return ""
	}
	if err := installBashyExecutable(cached, path); err != nil {
		slog.Warn("bashy auto-roll: install failed (keeping current bashy)", "err", err)
		return ""
	}
	slog.Info("bashy auto-rolled to match outpost", "version", want, "path", path)
	return path
}

// ReconcileExisting rolls an ALREADY-INSTALLED outpost-managed bashy to the
// matched version — but never installs one if absent. Called once at boot on
// every host (independent of whether any supervised bashy service is enabled),
// so the fleet auto-roll reaches hosts that run bashy only for deploy jobs, not
// as a supervised service. Best-effort; safe in a goroutine.
func (r *bashyBinaryResolver) ReconcileExisting(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reconciled {
		return
	}
	for _, cand := range bashyCandidatePaths() {
		if isExecutableFile(cand) && r.isManaged(cand) {
			r.reconciled = true
			if np := r.reconcile(ctx, cand); np != "" {
				r.cached = np
			}
			return
		}
	}
}

// isManaged reports whether a resolved bashy sits in an outpost-managed location
// (outpost-adjacent dir or the outpost cache) — the only bashy the resolver will
// auto-roll. A bashy the operator installed elsewhere on PATH is left untouched.
func (r *bashyBinaryResolver) isManaged(path string) bool {
	return bashypath.Managed(path, r.executable)
}

// bashyInstalledVersion runs `<path> --version` and extracts the bashy release
// from the "5.3.0(1)-bashy-<ver>" banner, or "" if it can't be read.
func bashyInstalledVersion(ctx context.Context, path string) string {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	return parseBashyBanner(string(out))
}

// parseBashyBanner extracts the bashy release from a `--version` banner like
// "GNU bash, version 5.3.0(1)-bashy-0.13.0", returning "0.13.0" (or "").
func parseBashyBanner(s string) string {
	i := strings.Index(s, "-bashy-")
	if i < 0 {
		return ""
	}
	rest := s[i+len("-bashy-"):]
	if end := strings.IndexFunc(rest, func(c rune) bool {
		return c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == '(' || c == ')'
	}); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}

// sameBashyVersion compares the product version (v and -dev are display/channel
// differences). Download selection always retains the exact release channel.
func sameBashyVersion(have, want string) bool {
	normalize := func(v string) string {
		return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(v), "v"), "-dev")
	}
	return normalize(have) == normalize(want)
}

// sibling returns the paired executable beside this outpost, following install links.
func (r *bashyBinaryResolver) sibling() string {
	return bashypath.Sibling(r.executable)
}

// bashyCandidatePaths lists the usual bashy install locations, checked when it
// is not on the daemon's (often narrow) PATH.
func bashyCandidatePaths() []string {
	return bashypath.Candidates()
}

// isExecutableFile reports whether path is a runnable file (regular file, and
// on unix carrying an execute bit).
func isExecutableFile(path string) bool {
	return bashypath.Executable(path)
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
		Use:   "bashy [-- shell-args...]",
		Short: "Download, verify, and cache the bashy system shell",
		Long: `outpost bashy seeds bashy onto a machine that already has outpost.

It resolves a qiangli/bashy GitHub release for this OS/arch, downloads and
verifies the archive using the release checksums, extracts bashy, and prints the
cached path. Pass --install PATH or --install-dir DIR to copy it into a stable
location after verification.

This command intentionally does not use system git, system bash, or system
coreutils. Once bashy exists, use bashy git / bashy dag / bashy self for the
rest of the build and update workflow.

Run the resolved shell with outpost bashy -- <args>, for example:
  outpost bashy -- -c 'echo hello'
Bare --version reports the paired shell; --version=TAG selects a bootstrap tag.`,
		Args: func(cmd *cobra.Command, args []string) error {
			// Preserve the historical --version TAG bootstrap spelling as well as
			// --version=TAG, while bare --version now reports the installed pair.
			if version == "__report__" && len(args) == 1 && cmd.ArgsLenAtDash() < 0 {
				version = args[0]
				return nil
			}
			return nil
		},
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			forward := cmd.ArgsLenAtDash() >= 0 || (len(args) > 0 && version == "")
			if forward {
				if install != "" || installDir != "" || (version != "" && version != "__report__") || cmd.Flags().Changed("repo") {
					return errors.New("shell arguments cannot be combined with bootstrap flags")
				}
				path, err := bashyResolver.Path(cmd.Context())
				if err != nil {
					return err
				}
				child := exec.CommandContext(cmd.Context(), path, args...)
				child.Stdin, child.Stdout, child.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
				return child.Run()
			}
			if version == "__report__" {
				path, err := bashyResolver.Path(cmd.Context())
				if err != nil {
					return err
				}
				child := exec.CommandContext(cmd.Context(), path, "--version")
				child.Stdin, child.Stdout, child.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
				return child.Run()
			}
			if install != "" && installDir != "" {
				return errors.New("--install and --install-dir are mutually exclusive")
			}
			path, err := ensureBashy(cmd.Context(), bashyResolveOptions{
				Repo: repo,
				Version: func() string {
					if version != "" {
						return version
					}
					return bashyResolver.effectiveVersion()
				}(),
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
	cmd.Flags().StringVar(&version, "version", "", "Report bashy version, or fetch the specified release tag")
	cmd.Flags().Lookup("version").NoOptDefVal = "__report__"

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
	extension := ".tar.gz"
	if strings.EqualFold(goos, "windows") {
		extension = ".zip"
	}
	return strings.EqualFold(name, "bashy-"+goos+"-"+goarch+extension)
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
		if runtime.GOOS != "windows" {
			return err
		}
		if err := replaceBashyAside(tmpName, dst); err != nil {
			return err
		}
	}
	removeTmp = false
	return nil
}

// replaceBashyAside supports replacing an executing Windows image. Restore the
// original path if installation fails; keep the old image until it can be deleted.
func replaceBashyAside(tmp, dst string) error {
	old := dst + ".old"
	if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
		f, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".old-*")
		if err != nil {
			return err
		}
		old = f.Name()
		_ = f.Close()
		_ = os.Remove(old)
	}
	if err := os.Rename(dst, old); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		if restore := os.Rename(old, dst); restore != nil {
			return fmt.Errorf("install: %w; restore: %v", err, restore)
		}
		return err
	}
	return nil
}
