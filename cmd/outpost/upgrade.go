package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/upgrade"
)

// outpost upgrade — swap the running daemon's binary in place and ask
// it to re-exec on the new build. Replaces the prior multi-step
// scp/stop/mv/start/poll dance with one verb.
//
// Source: --local PATH (copy) or --from HTTPS-URL (download). Target
// path comes from the running daemon's reported BinaryPath, so the
// caller can drive an upgrade purely through MCP without having to
// guess where outpost is installed.
//
// Atomic swap: candidate lands at "<binary>.upgrading" next to the
// running binary (same filesystem → rename is atomic), gets chmod 0755,
// gets probed by exec-ing `<candidate> version --json` and parsing the
// BuildInfo. If the probe is malformed or the commit matches the live
// build (and --force wasn't passed), the candidate is removed without
// touching the live binary. Otherwise os.Rename swaps it in.
//
// Restart: by default calls outpost_restart over MCP and polls
// outpost://status until the reported Build.Commit matches the new
// binary's commit (30s timeout). --no-restart leaves the on-disk swap
// in place but doesn't touch the running daemon — useful for staging.
//
// Windows: refused for now. os.Rename over a running .exe fails on
// Windows; the right pattern is rename-old-out-then-new-in, which we
// can add when there's a real Windows user. Mac/Linux are the targets.
func upgradeCmd() *cobra.Command {
	var (
		fromURL   string
		localPath string
		sha256Hex string
		force     bool
		noRestart bool
		waitFor   time.Duration
	)
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Swap the running daemon's binary in place and re-exec",
		Long: `outpost upgrade replaces the running daemon's binary atomically and
asks it to re-exec on the new build.

Examples:
  outpost upgrade --local ./bin/outpost
  outpost upgrade --from https://releases.example.com/outpost-darwin-arm64
  outpost upgrade --from https://... --sha256 <hex>      # verify download
  outpost upgrade --local ./bin/outpost --no-restart     # swap only

The candidate binary is verified by exec'ing "<candidate> version --json"
before the swap. Same-commit upgrades are a no-op unless --force is passed.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (fromURL == "") == (localPath == "") {
				return errors.New("exactly one of --from or --local is required")
			}
			ctx := cmd.Context()

			// Phase 1: ask the daemon what it is and where it lives.
			before, err := readStatus(ctx)
			if err != nil {
				return err
			}
			if before.BinaryPath == "" {
				return errors.New("daemon did not report binary_path — running an older outpost that predates `outpost upgrade`; please update by hand once")
			}
			fmt.Printf("running:  %s at %s\n", before.Build.Short(), before.BinaryPath)

			// Phase 2: stage the candidate at "<binary>.upgrading" on the same
			// filesystem as the target so the final rename is atomic.
			candidate := before.BinaryPath + ".upgrading"
			if err := stageCLICandidate(ctx, candidate, fromURL, localPath, sha256Hex); err != nil {
				_ = os.Remove(candidate)
				return err
			}
			// From here on, remove the candidate on any error.
			swapped := false
			defer func() {
				if !swapped {
					_ = os.Remove(candidate)
				}
			}()

			// Phase 3: probe the candidate. This is the gate that keeps a
			// cross-arch or wholly unrelated binary from clobbering the live
			// one. CLI doesn't pre-commit to a sha, so pass "".
			newBuild, err := upgrade.Probe(candidate, "")
			if err != nil {
				return fmt.Errorf("verify candidate: %w", err)
			}
			fmt.Printf("candidate: %s (%s)\n", newBuild.Short(), newBuild.GoVersion)

			if newBuild.Commit != "" && newBuild.Commit == before.Build.Commit && !force {
				return fmt.Errorf("candidate is the same commit as the running daemon (%s) — pass --force to upgrade anyway", before.Build.Short())
			}

			// Phase 3.5: hardlink current → outpost.previous for rollback.
			// Same retention contract the daemon Worker uses; keeping the
			// CLI consistent so `outpost rollback` works after either path.
			previous := before.BinaryPath + ".previous"
			if err := upgrade.RetainPrevious(before.BinaryPath, previous); err != nil {
				// Non-fatal — log and proceed. Rollback won't be available
				// for this upgrade, but the upgrade itself can still
				// complete. Mirrors the daemon Worker's behavior.
				fmt.Printf("previous: WARN couldn't retain rollback target: %v\n", err)
			}

			// Phase 4: atomic swap. SwapAtomic is one rename on Unix (the
			// kernel allows overwriting the running binary's path) and
			// rename-old-out-then-new-in on Windows (which doesn't allow
			// the one-shot variant). After this point os.Executable() on
			// the daemon still resolves to the same path; subsequent
			// execs (via the self-restart path) pick up the new binary.
			if err := upgrade.SwapAtomic(before.BinaryPath, candidate); err != nil {
				return fmt.Errorf("swap binary into place: %w", err)
			}
			swapped = true
			fmt.Printf("swapped:  %s → %s\n", before.Build.Short(), newBuild.Short())

			// Append a ledger entry so `outpost upgrade history` reflects
			// CLI-driven swaps alongside cloudbox-pushed ones. Cache dir
			// resolution mirrors the daemon's wiring in main.go.
			if cacheDir, err := conf.ResolveCacheDir(); err == nil && cacheDir != "" {
				_ = upgrade.NewLedger(filepath.Join(cacheDir, "upgrade.log")).Append(upgrade.LedgerEntry{
					Step:    "swap_done",
					FromSHA: before.Build.Short(),
					ToSHA:   newBuild.Short(),
					Detail:  "outpost upgrade (CLI)",
				})
			}

			if noRestart {
				fmt.Println("--no-restart set; daemon still running old build. Run `outpost restart` when ready.")
				return nil
			}

			// Phase 5: trigger re-exec.
			if err := restartViaMCP(ctx); err != nil {
				return fmt.Errorf("trigger restart: %w", err)
			}
			fmt.Println("restart:  scheduled")

			// Phase 6: poll until the daemon comes back on the new build.
			after, err := waitForBuild(ctx, newBuild.Commit, waitFor)
			if err != nil {
				return fmt.Errorf("waiting for daemon to come back: %w (binary is swapped — investigate with `outpost status`)", err)
			}
			fmt.Printf("ready:    %s at %s\n", after.Build.Short(), after.BinaryPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&fromURL, "from", "", "HTTPS URL to download the candidate binary from")
	cmd.Flags().StringVar(&localPath, "local", "", "Local path to a candidate outpost binary")
	cmd.Flags().StringVar(&sha256Hex, "sha256", "", "Expected sha256 (hex) of the candidate — required-recommended for --from")
	cmd.Flags().BoolVar(&force, "force", false, "Swap even when candidate commit matches the running build")
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "Swap binary on disk but do not trigger restart")
	cmd.Flags().DurationVar(&waitFor, "wait", 30*time.Second, "Max time to wait for the daemon to come back on the new build")
	cmd.AddCommand(upgradeHistoryCmd(), upgradeApplyCmd())
	return cmd
}

// readStatus is a thin one-shot wrapper: dial MCP, read
// outpost://status, close. Each phase of upgrade opens its own session
// because the restart phase intentionally tears the connection down.
func readStatus(ctx context.Context) (admincore.StatusView, error) {
	session, err := dialMCP(ctx)
	if err != nil {
		return admincore.StatusView{}, err
	}
	defer session.close()
	var st admincore.StatusView
	if err := session.readResource(ctx, "outpost://status", &st); err != nil {
		return admincore.StatusView{}, err
	}
	return st, nil
}

func restartViaMCP(ctx context.Context) error {
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	return session.callTool(ctx, "outpost_restart", map[string]any{}, nil)
}

// stageCLICandidate picks between the two CLI source modes (HTTPS URL
// vs local path) and delegates to the corresponding upgrade-package
// helper. The daemon worker never touches this — it only stages from
// URLs delivered via the wire envelope.
func stageCLICandidate(ctx context.Context, dst, fromURL, localPath, expectedSHA string) error {
	switch {
	case fromURL != "":
		return upgrade.StageFromURL(ctx, dst, fromURL, expectedSHA, nil)
	case localPath != "":
		return upgrade.StageFromLocal(localPath, dst, expectedSHA)
	}
	return errors.New("internal: stageCLICandidate called with neither source")
}

// waitForBuild polls outpost://status until the reported Build.Commit
// matches `want` or the deadline elapses. Errors and connection
// refusals during the restart window are expected — we keep retrying.
func waitForBuild(ctx context.Context, want string, max time.Duration) (admincore.StatusView, error) {
	deadline := time.Now().Add(max)
	// Brief grace period so the parent has time to start the child
	// before we probe — saves a couple of noisy retries.
	time.Sleep(300 * time.Millisecond)
	for {
		if time.Now().After(deadline) {
			return admincore.StatusView{}, fmt.Errorf("timed out after %s", max)
		}
		st, err := readStatus(ctx)
		if err == nil && st.Build.Commit == want {
			return st, nil
		}
		select {
		case <-ctx.Done():
			return admincore.StatusView{}, ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
}
