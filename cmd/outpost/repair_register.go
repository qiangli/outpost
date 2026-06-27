// `outpost repair register` — peer-driven re-registration of a
// BROKEN remote outpost from the local (healthy) outpost.
//
// Counterpart to `outpost peers help-mint-invite`:
//
//   1. operator runs `outpost peers help-mint-invite <broken>` on a
//      healthy peer → cloudbox returns a one-time code
//   2. operator runs `outpost repair register --to <broken>
//      --code <code> --name <broken>` on the same healthy peer
//      → this command sshs into the broken peer and runs
//      `outpost register --code <code> --name <broken>` there,
//      using the cloudbox URL the LOCAL outpost is paired with.
//
// Trust model: the broken peer must already be a configured SSH
// target on the local outpost. The local outpost passes the bearer
// scope `host:invite-mint`'s short-lived code through the ssh tunnel
// it already has open — cloudbox sees a normal Exchange call from
// the broken peer. No new trust path opens.
//
// This is the resilience-Layer-3 path the roadmap explicitly deferred
// to Phase 2 ("Peer-mediated re-registration"); promoted back into
// Wave 3A.2 after the host-b 2026-05-22 incident exposed the
// chicken-and-egg in the original deferral.

package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func repairRegisterCmd() *cobra.Command {
	var (
		toFlag     string
		codeFlag   string
		nameFlag   string
		serverFlag string
	)
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Drive `outpost register` on a broken peer via SSH using a freshly-minted invite code",
		Long: `Runs 'outpost register --code <code> --name <name> --server <url>'
on a broken peer via SSH. The invite code typically comes from
'outpost peers help-mint-invite <peer>' on this same healthy
outpost, but a code from any source (SPA copy, another peer)
works the same way.

The peer must already be a configured SSH target (run
'outpost ssh add' first). When the broken peer is unreachable
directly, configure 'outpost ssh add --via <jump-hop>' to chain
through a healthy hop.

The --server URL defaults to the cloudbox URL this LOCAL outpost
is paired with — the operator can override when re-homing the
peer to a different cloudbox account.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if toFlag == "" {
				return fmt.Errorf("--to <peer> is required (the SSH target alias of the broken peer)")
			}
			if codeFlag == "" {
				return fmt.Errorf("--code <invite> is required (mint one via 'outpost peers help-mint-invite')")
			}
			if nameFlag == "" {
				// Default the host name to the peer alias — operator
				// gets the same naming on both sides without thinking.
				nameFlag = toFlag
			}
			return runRepairRegister(cmd.Context(), toFlag, codeFlag, nameFlag, serverFlag)
		},
	}
	cmd.Flags().StringVar(&toFlag, "to", "", "SSH target alias of the broken peer. Required.")
	cmd.Flags().StringVar(&codeFlag, "code", "", "One-time invite code from cloudbox. Required.")
	cmd.Flags().StringVar(&nameFlag, "name", "", "Host name to register as (default: same as --to alias)")
	cmd.Flags().StringVar(&serverFlag, "server", "", "Cloudbox base URL (default: this outpost's cloudbox)")
	return cmd
}

func runRepairRegister(ctx context.Context, peerName, code, name, serverURL string) error {
	// Default cloudbox URL from local FileConfig — the operator
	// typically wants the peer to re-pair with the SAME cloudbox
	// the rest of the fleet is on.
	if serverURL == "" {
		cfgPath, err := conf.DefaultConfigPath()
		if err != nil {
			return fmt.Errorf("config path: %w", err)
		}
		fc, err := conf.LoadFile(cfgPath)
		if err != nil || fc == nil {
			return fmt.Errorf("load %s: %w (pass --server explicitly when local outpost is unpaired)", cfgPath, err)
		}
		serverURL = cloudboxHTTPBase(fc)
		if serverURL == "" {
			return fmt.Errorf("cannot derive cloudbox URL from local fc; pass --server explicitly")
		}
	}

	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()

	// Compose the remote command. --yes self-confirms (no prompt);
	// register's existing flow handles the rest (Exchange → SaveFile
	// → re-exec as a detached child via execSelfStart). We don't
	// touch the broken peer's existing config beyond what register
	// itself writes.
	remoteCmd := fmt.Sprintf(
		"outpost register --server %s --code %s --name %s --yes",
		shellQuote(serverURL),
		shellQuote(code),
		shellQuote(name),
	)
	fmt.Printf("Driving on %s: %s\n", peerName, remoteCmd)

	var res sshExecResult
	if err := session.callTool(ctx, "outpost_ssh_exec", map[string]any{
		"name":            peerName,
		"command":         remoteCmd,
		"timeout_seconds": 120,
		"max_stdout":      256 * 1024,
		"max_stderr":      256 * 1024,
	}, &res); err != nil {
		return fmt.Errorf("ssh exec on %s: %w", peerName, err)
	}
	if res.Stdout != "" {
		fmt.Print(res.Stdout)
		if !strings.HasSuffix(res.Stdout, "\n") {
			fmt.Println()
		}
	}
	if res.Stderr != "" {
		fmt.Print(res.Stderr)
		if !strings.HasSuffix(res.Stderr, "\n") {
			fmt.Println()
		}
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("peer 'outpost register' exited %d", res.ExitCode)
	}
	fmt.Printf("\nOK — %s is re-paired with %s as %q.\n", peerName, serverURL, name)
	fmt.Printf("Verify with:\n  outpost ssh exec %s -- outpost status\n", peerName)
	return nil
}
