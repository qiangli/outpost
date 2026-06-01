// `outpost repair remote-binary` — peer-driven recovery of a BROKEN
// remote outpost from the local (healthy) outpost.
//
// Symmetric to `outpost repair binary --from <peer>` (which fetches
// the peer's binary to fix LOCAL state). This one pushes the LOCAL
// binary to a REMOTE peer that has hit a broken-binary state but
// still has a working SSH path (LAN-direct, cloudbox WSS fallback,
// or chained via a known-healthy hop).
//
// Mechanics: open the local binary via os.Executable(), stream it
// over the existing outpost_ssh_exec MCP tool with stdin_b64 set,
// land it at a candidate path on the peer, and (when --apply is
// passed) exec `outpost upgrade --local <candidate> --yes` on the
// peer to swap + restart.
//
// The peer must already be a configured SSH target on this local
// outpost (`outpost ssh add <name>`). For a peer that's lost LAN
// reachability AND cloudbox connectivity, the operator first uses a
// known-good chain (`outpost ssh add --via <jump-hop> ...`) so the
// SSH client can hop to the broken peer through a healthy one.

package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func repairRemoteBinaryCmd() *cobra.Command {
	var (
		toFlag         string
		applyFlag      bool
		remotePathFlag string
		localBinFlag   string
	)
	cmd := &cobra.Command{
		Use:   "remote-binary",
		Short: "Push the LOCAL outpost binary to a REMOTE peer (and optionally apply via upgrade)",
		Long: `Streams the local outpost binary (os.Executable() or --local override)
to <peer> via SSH and lands it at a candidate path on the peer
(default ~/.cache/outpost/repair/outpost-incoming). When --apply
is passed, also runs 'outpost upgrade --local <candidate> --yes'
on the peer to swap + restart.

Use case: a remote outpost has hit a broken-binary state (corrupt
self-upgrade, accidental rm of the binary, etc.) but its SSH
surface is still reachable from a healthy peer. This recipe is
the inverse of 'repair binary --from <peer>' — instead of pulling
a fix from a healthy box, you push the fix INTO the broken box.

The peer must already be a configured SSH target (run
'outpost ssh add' first). When the broken peer is unreachable
directly, configure 'outpost ssh add --via <jump-hop>' so the
SSH client chains through a healthy hop.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if toFlag == "" {
				return fmt.Errorf("--to <peer> is required (the SSH target alias of the broken peer)")
			}
			return runRepairRemoteBinary(cmd.Context(), toFlag, localBinFlag, remotePathFlag, applyFlag)
		},
	}
	cmd.Flags().StringVar(&toFlag, "to", "", "SSH target alias of the peer to repair. Required.")
	cmd.Flags().StringVar(&localBinFlag, "local", "", "Local binary to push (default: this outpost's binary, via os.Executable())")
	cmd.Flags().StringVar(&remotePathFlag, "remote-path", "", "Remote candidate path (default: ~/.cache/outpost/repair/outpost-incoming)")
	cmd.Flags().BoolVar(&applyFlag, "apply", false, "After landing, run 'outpost upgrade --local <candidate> --yes' on the peer")
	return cmd
}

func runRepairRemoteBinary(ctx context.Context, peerName, localBin, remotePath string, apply bool) error {
	// Step 1: resolve the local binary path.
	if localBin == "" {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate own binary: %w", err)
		}
		localBin = self
	}
	bin, err := os.ReadFile(localBin)
	if err != nil {
		return fmt.Errorf("read local binary %s: %w", localBin, err)
	}
	fmt.Fprintf(os.Stderr, "Read %d bytes from %s\n", len(bin), localBin)

	// Step 2: default the remote candidate path. ~ expands on the
	// remote side via the shell — the SSH server's login shell does
	// the expansion when it runs the Command.
	if remotePath == "" {
		remotePath = "~/.cache/outpost/repair/outpost-incoming"
	}

	// Step 3: stream the binary over SSH. base64-encode locally; the
	// peer command `base64 -d > <path>` decodes back to bytes. mkdir
	// -p the directory in the same command so a fresh peer doesn't
	// need any pre-setup.
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()

	stdinB64 := base64.StdEncoding.EncodeToString(bin)

	dir := filepath.Dir(remotePath)
	// Use a heredoc-free single-line shell to make quoting safe.
	pushCmd := fmt.Sprintf(
		"mkdir -p %s && base64 -d > %s.partial && chmod +x %s.partial && mv %s.partial %s",
		shellQuote(dir), shellQuote(remotePath), shellQuote(remotePath), shellQuote(remotePath), shellQuote(remotePath),
	)
	fmt.Fprintf(os.Stderr, "Streaming %d-byte base64 to %s:%s ...\n", len(stdinB64), peerName, remotePath)
	var push sshExecResult
	if err := session.callTool(ctx, "outpost_ssh_exec", map[string]any{
		"name":            peerName,
		"command":         pushCmd,
		"timeout_seconds": 600,
		"stdin_b64":       stdinB64,
		"max_stdout":      256 * 1024,
		"max_stderr":      256 * 1024,
	}, &push); err != nil {
		return fmt.Errorf("push binary: %w", err)
	}
	if push.ExitCode != 0 {
		fmt.Fprint(os.Stderr, push.Stderr)
		return fmt.Errorf("peer's base64-decode exited %d", push.ExitCode)
	}
	fmt.Fprintf(os.Stderr, "Landed at %s on %s\n", remotePath, peerName)

	// Step 4: probe the candidate before applying — `version --json`
	// proves the binary actually executes on the peer's OS/arch (a
	// mismatch is the most common foot-gun when pushing across
	// platforms).
	var probe sshExecResult
	probeCmd := shellQuote(remotePath) + " version --json"
	if err := session.callTool(ctx, "outpost_ssh_exec", map[string]any{
		"name":            peerName,
		"command":         probeCmd,
		"timeout_seconds": 30,
	}, &probe); err != nil {
		return fmt.Errorf("probe candidate on peer: %w", err)
	}
	if probe.ExitCode != 0 {
		fmt.Fprint(os.Stderr, probe.Stderr)
		return fmt.Errorf("candidate refused to report version (exit %d) — likely OS/arch mismatch", probe.ExitCode)
	}
	fmt.Fprintf(os.Stderr, "Candidate self-check: %s\n", trimSpace(probe.Stdout))

	// Step 5 (optional): apply via the existing upgrade flow on the
	// peer. `outpost upgrade --local <path> --yes` runs the same
	// stage → probe → swap → restart machinery used by the cloudbox
	// fleet-upgrade fan-out; we just kick it via SSH instead of the
	// matrix tunnel.
	if !apply {
		fmt.Fprintf(os.Stderr, "\nPeer-side apply skipped (--apply not passed).\n")
		fmt.Fprintf(os.Stderr, "Run manually on %s with:\n", peerName)
		fmt.Fprintf(os.Stderr, "  outpost upgrade --local %s --yes\n", remotePath)
		return nil
	}
	applyCmd := "outpost upgrade --local " + shellQuote(remotePath) + " --yes"
	fmt.Fprintf(os.Stderr, "Applying on %s: %s\n", peerName, applyCmd)
	var apl sshExecResult
	if err := session.callTool(ctx, "outpost_ssh_exec", map[string]any{
		"name":            peerName,
		"command":         applyCmd,
		"timeout_seconds": 120,
	}, &apl); err != nil {
		return fmt.Errorf("apply on peer: %w", err)
	}
	if apl.Stdout != "" {
		fmt.Fprint(os.Stderr, apl.Stdout)
	}
	if apl.Stderr != "" {
		fmt.Fprint(os.Stderr, apl.Stderr)
	}
	if apl.ExitCode != 0 {
		return fmt.Errorf("peer 'outpost upgrade' exited %d", apl.ExitCode)
	}
	fmt.Fprintf(os.Stderr, "Apply OK — peer %s will restart momentarily.\n", peerName)
	return nil
}
