package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// outpost unpair — clear the portal pairing (AgentName, Token,
// AccessToken). Apps + outbound mounts + builtin toggles are preserved.
// Available only via MCP (no UI equivalent yet); the daemon restarts to
// drop the matrix tunnel.
func unpairCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "unpair",
		Short: "Clear the portal pairing (keeps apps/outbound/builtins). Daemon restarts.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				fmt.Println("This will clear AgentName, Token, AccessToken from agent.json and restart outpost.")
				fmt.Println("Apps, outbound mounts, and built-in toggles are preserved.")
				fmt.Println("Re-run with --yes to confirm.")
				return nil
			}
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				RestartPending bool `json:"restart_pending"`
			}
			if err := session.callTool(cmd.Context(), "outpost_unpair", map[string]any{}, &out); err != nil {
				return err
			}
			fmt.Println("Unpaired. Restarting outpost — poll `outpost status` until configured=false.")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	return cmd
}
