package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// outpost restart — ask the running daemon to re-exec itself. Uses the
// same restart closure adminui hits when an operator flips a builtin.
// Cleaner than `outpost stop && outpost start` because it stays in the
// background-daemon pattern without re-execing the supervisor.
func restartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Ask the running outpost daemon to re-exec itself",
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			if err := session.callTool(cmd.Context(), "outpost_restart", map[string]any{}, nil); err != nil {
				return err
			}
			fmt.Println("Restart scheduled — poll `outpost status` until configured returns.")
			return nil
		},
	}
	return cmd
}
