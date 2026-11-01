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
		err = applyPeerRepair(cmd.Context(), *peer, env, execPeerRepair)
		if err != nil {
			_ = r.FinishTarget(env, err)
			return err
		}
		return r.Observe(agent.BuildInfo{Version: peer.Version}, env, false, "", true)
	}}
	cmd.Flags().BoolVar(&approve, "approve", false, "Approve the resolved release and run the peer's own upgrade CLI")
	return cmd
}

// Arguments are quoted for the peer exec path's shell, on every OS.
func peerRepairQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func applyPeerRepair(ctx context.Context, peer peerstatus.Peer, env upgrade.Envelope, run peerRepairExec) error {
	if !peer.Owned || peer.Shared {
		return errors.New("upgrade requires an owned peer")
	}
	if err := upgrade.ValidateRepairTarget(env); err != nil {
		return err
	}
	command := "outpost upgrade --from " + peerRepairQuote(env.URL) + " --sha256 " + peerRepairQuote(env.SHA256)
	if _, err := run(ctx, peer.Host, command); err != nil {
		return err
	}
	// Query the daemon, not just the executable: an on-disk swap alone is not success.
	raw, err := run(ctx, peer.Host, "outpost status --json")
	if err != nil {
		return fmt.Errorf("recheck peer: %w", err)
	}
	var response struct {
		Status admincore.StatusView `json:"status"`
	}
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return fmt.Errorf("decode peer status: %w", err)
	}
	if response.Status.Build.Version != env.ReleaseID {
		return fmt.Errorf("peer version recheck: got %s, want %s", response.Status.Build.Version, env.ReleaseID)
	}
	return nil
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
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("peer command exited %d: %s", result.ExitCode, result.Stderr)
	}
	return result.Stdout, nil
}

// Bind the SSH alias to the owned board entry. A same-named alias may point
// elsewhere, and a hop-side address is not proof of ownership of that endpoint.
func peerUpgradeTarget(host string, targets []admincore.SSHTargetView) (string, error) {
	for _, target := range targets {
		if target.Host == host && target.Via == "" {
			return target.Name, nil
		}
	}
	return "", errors.New("owned peer needs a direct configured SSH target; use outpost ssh add and outpost connect first")
}
