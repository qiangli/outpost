package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type rollbackOut struct {
	OK         bool   `json:"ok"`
	Status     string `json:"status,omitempty"`
	Detail     string `json:"detail,omitempty"`
	FromCommit string `json:"from_commit,omitempty"`
	ToCommit   string `json:"to_commit,omitempty"`
}

// rollbackCmd asks the daemon to restore outpost.previous over the
// live binary and re-exec. Mirrors `outpost restart` in shape: thin
// MCP call, terse output, exits non-zero on refusal so an automation
// wrapper notices.
func rollbackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Restore the previously-running outpost binary and re-exec",
		Long: `Swap outpost.previous back over the live binary and trigger a daemon
restart. Useful when a freshly-pushed upgrade misbehaved and you want
to bail back to the prior build without re-downloading it.

Refuses when no outpost.previous is on disk (the daemon has never
been upgraded since this binary was placed there manually) or when
another upgrade is currently in flight.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out rollbackOut
			if err := session.callTool(cmd.Context(), "outpost_rollback", map[string]any{}, &out); err != nil {
				if strings.Contains(err.Error(), "unknown tool") {
					return fmt.Errorf("rollback is only available on paired hosts (the upgrade surface only registers once cloudbox has issued an access_token)")
				}
				return err
			}
			switch out.Status {
			case "no_previous":
				return fmt.Errorf("no outpost.previous on disk — this daemon has never been upgraded, nothing to roll back to")
			case "in_flight":
				return fmt.Errorf("an upgrade is currently running; wait for it to finish, then retry")
			case "invalid":
				return fmt.Errorf("rollback refused: %s", out.Detail)
			}
			fmt.Printf("rollback: %s → %s\n", out.FromCommit, out.ToCommit)
			fmt.Println("restart scheduled — poll `outpost status` until configured returns.")
			return nil
		},
	}
	return cmd
}
