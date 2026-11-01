package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/peerstatus"
	"github.com/qiangli/outpost/internal/agent/upgrade"
	"github.com/spf13/cobra"
)

type peerRepairExec func(context.Context, string, string) (string, error)

// peerRepairUncertain marks an error after the execution request was sent.
type peerRepairUncertain struct{ error }

func finishPeerRepair(ctx context.Context, peer peerstatus.Peer, env upgrade.Envelope, ledger upgrade.RepairLedger, run peerRepairExec, timeout, interval time.Duration) error {
	err := applyPeerRepairWithPolling(ctx, peer, env, run, timeout, interval)
	return errors.Join(err, ledger.FinishTarget(env, err))
}

func peersUpgradeCmd() *cobra.Command {
	var approve bool
	cmd := &cobra.Command{Use: "upgrade <host>", Short: "Resolve an owned peer upgrade; --approve applies and verifies it", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		peers, err := fetchPeersStatus(cmd.Context())
		if err != nil {
			return err
		}
		var peer *peerstatus.Peer
		for i := range peers {
			if peers[i].Host == args[0] || peers[i].Alias == args[0] {
				if peer != nil {
					return errors.New("ambiguous peer")
				}
				peer = &peers[i]
			}
		}
		if peer == nil || !peer.Owned || peer.Shared {
			return errors.New("upgrade requires an owned peer")
		}
		env, err := upgrade.GitHubSource{Platform: peer.OS + "_" + peer.Arch, Current: agent.BuildInfo{Version: peer.Version, Commit: peer.Commit}, Token: githubUpgradeToken()}.Resolve(cmd.Context())
		if err != nil {
			return err
		}
		path, err := conf.DefaultConfigPath()
		if err != nil {
			return err
		}
		r := upgrade.RepairLedger{Path: filepath.Join(filepath.Dir(path), "heals.jsonl"), Peer: peer.Host}
		if err := r.Observe(agent.BuildInfo{Version: peer.Version}, env, false, "", false); err != nil {
			return err
		}
		if !approve {
			fmt.Fprintf(cmd.OutOrStdout(), "Resolved %s with sha256 %s; run peers upgrade <host> --approve to apply.\n", env.ReleaseID, env.SHA256)
			return nil
		}
		if err := finishPeerRepair(cmd.Context(), *peer, env, r, execPeerRepair, 3*time.Minute, 3*time.Second); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Upgraded %s from %s to %s; verified target with sha256 %s.\n", peer.Host, peer.Version, env.ReleaseID, env.SHA256)
		return nil
	}}
	cmd.Flags().BoolVar(&approve, "approve", false, "Approve the resolved release and run the peer's own upgrade CLI")
	return cmd
}

// Arguments are quoted for the peer exec path's shell, on every OS.
func peerRepairQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func applyPeerRepair(ctx context.Context, peer peerstatus.Peer, env upgrade.Envelope, run peerRepairExec) error {
	return applyPeerRepairWithPolling(ctx, peer, env, run, 3*time.Minute, 3*time.Second)
}

func applyPeerRepairWithPolling(ctx context.Context, peer peerstatus.Peer, env upgrade.Envelope, run peerRepairExec, timeout, interval time.Duration) error {
	if !peer.Owned || peer.Shared {
		return errors.New("upgrade requires an owned peer")
	}
	if err := upgrade.ValidateRepairTarget(env); err != nil {
		return err
	}
	command := "outpost upgrade --from " + peerRepairQuote(env.URL) + " --sha256 " + peerRepairQuote(env.SHA256)
	if _, err := run(ctx, peer.Host, command); err != nil {
		var uncertain peerRepairUncertain
		if !errors.As(err, &uncertain) {
			return err
		}
	}
	verifyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var version string
	var lastErr error
	for {
		// Each call reconnects; bound individual requests as well as the whole window.
		queryCtx, queryCancel := context.WithTimeout(verifyCtx, 15*time.Second)
		var commit string
		version, commit, lastErr = peerRepairVersion(queryCtx, peer.Host, run)
		queryCancel()
		if lastErr == nil && peerRunsTarget(version, commit, env) {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-verifyCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if lastErr != nil {
				return fmt.Errorf("peer unreachable during upgrade verification: %w", lastErr)
			}
			if version == peer.Version {
				return fmt.Errorf("peer auto-rolled back or not applied: still reports %s after verification timeout", version)
			}
			return fmt.Errorf("peer version recheck timed out: got %s, want %s", version, env.ReleaseID)
		case <-timer.C:
		}
	}
}

func peerRepairVersion(ctx context.Context, host string, run peerRepairExec) (string, string, error) {
	// Query the daemon: an on-disk swap alone is not success.
	raw, err := run(ctx, host, "outpost status --json")
	if err != nil {
		return "", "", err
	}
	var response struct {
		Status admincore.StatusView `json:"status"`
	}
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return "", "", fmt.Errorf("decode peer status: %w", err)
	}
	if response.Status.Build.Version == "" {
		return "", "", errors.New("peer status has no running version")
	}
	return response.Status.Build.Version, response.Status.Build.Commit, nil
}

// peerRunsTarget reports whether the peer's running build is the target.
// The build commit is the identity when both sides carry one (short or
// full sha); a release build reports its tag with a "-dev" suffix, so the
// version comparison ignores that suffix only when no commit is available.
func peerRunsTarget(version, commit string, env upgrade.Envelope) bool {
	want, have := strings.ToLower(strings.TrimSpace(env.Commit)), strings.ToLower(strings.TrimSpace(commit))
	if want != "" && have != "" {
		return strings.HasPrefix(have, want) || strings.HasPrefix(want, have)
	}
	return version == env.ReleaseID || strings.TrimSuffix(version, "-dev") == env.ReleaseID
}

func execPeerRepair(ctx context.Context, host, command string) (string, error) {
	session, err := dialMCP(ctx)
	if err != nil {
		return "", err
	}
	defer session.close()
	var targets struct {
		Targets []admincore.SSHTargetView `json:"targets"`
	}
	if err := session.callTool(ctx, "outpost_list_ssh_targets", map[string]any{}, &targets); err != nil {
		return "", err
	}
	name, err := peerUpgradeTarget(host, targets.Targets)
	if err != nil {
		return "", err
	}
	var result sshExecResult
	if err := session.callTool(ctx, "outpost_ssh_exec", map[string]any{"name": name, "command": command, "timeout_seconds": int((5 * time.Minute).Seconds())}, &result); err != nil {
		return "", peerRepairUncertain{err}
	}
	return peerRepairExecResult(result)
}

func peerRepairExecResult(result sshExecResult) (string, error) {
	if result.ExitCode == -1 && strings.TrimSpace(result.Stderr) == "" {
		return result.Stdout, peerRepairUncertain{errors.New("peer session ended without an exit status")}
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("peer command exited %d: %s", result.ExitCode, result.Stderr)
	}
	return result.Stdout, nil
}

// Bind the SSH alias to the owned board entry. A same-named alias may point
// elsewhere; only a matching configured Host identifies the owned endpoint.
func peerUpgradeTarget(host string, targets []admincore.SSHTargetView) (string, error) {
	for _, target := range targets {
		if target.Host == host {
			return target.Name, nil
		}
	}
	return "", fmt.Errorf("no SSH alias for %s; add one with: outpost ssh add %s --host %s", host, host, host)
}
