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

// DefaultBashyVersion is the bashy release this outpost was BUILT AND TESTED
// against — the two are versioned as a matched pair. When no explicit
// bashy_version pin is set, the supervisor reconciles the outpost-managed bashy
// to THIS version on boot. Because outpost itself auto-rolls across the fleet
// (fleet-upgrade push/pull), bumping this constant + releasing outpost is what
// rolls the matched bashy out to every host: outpost upgrades, restarts, and its
// first supervisor tick installs the matching bashy. Bump it whenever outpost is
// validated against a new bashy release.
const DefaultBashyVersion = "v0.13.0"

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
	found, managed := "", false
	if p := strings.TrimSpace(os.Getenv("OUTPOST_BASHY_BIN")); p != "" && isExecutableFile(p) {
		found = p // 1. operator override — used as-is
	} else if p, err := exec.LookPath("bashy"); err == nil {
		found, managed = p, r.isManaged(p) // 2. PATH
	} else {
		for _, cand := range bashyCandidatePaths() { // 3. common install locations
			if isExecutableFile(cand) {
				found, managed = cand, r.isManaged(cand)
				break
			}
		}
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
	return DefaultBashyVersion
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
	dir := filepath.Clean(filepath.Dir(path))
	if exe, err := os.Executable(); err == nil && dir == filepath.Clean(filepath.Dir(exe)) {
		return true
	}
	if cache, err := os.UserCacheDir(); err == nil && dir == filepath.Clean(filepath.Join(cache, "outpost", "bin")) {
		return true
	}
	return false
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

// sameBashyVersion compares release strings ignoring a leading "v" (the tag is
// "v0.13.0"; the banner reports "0.13.0").
func sameBashyVersion(have, want string) bool {
	return strings.TrimPrefix(strings.TrimSpace(have), "v") == strings.TrimPrefix(strings.TrimSpace(want), "v")
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
