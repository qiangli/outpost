package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/upgrade"
)

type upgradeHistoryResource struct {
	Entries []upgrade.LedgerEntry `json:"entries"`
}

// upgradeHistoryCmd renders the per-host upgrade ledger. JSON mode is
// for scripts; the human table is the at-a-glance "did the cloudbox
// push actually land here?" view that complements `outpost status`'s
// "where am I now" view.
func upgradeHistoryCmd() *cobra.Command {
	var (
		jsonOut bool
		limit   int
	)
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show this host's upgrade ledger (one entry per phase of every upgrade)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var payload upgradeHistoryResource
			if err := session.readResource(cmd.Context(), "outpost://upgrade-history", &payload); err != nil {
				if strings.Contains(err.Error(), "Resource not found") {
					return fmt.Errorf("upgrade history is only available on paired hosts (the upgrade ledger only mounts once cloudbox has issued an access_token)")
				}
				return err
			}
			entries := payload.Entries
			if limit > 0 && len(entries) > limit {
				entries = entries[len(entries)-limit:]
			}
			if jsonOut {
				b, _ := json.MarshalIndent(map[string]any{"entries": entries}, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if len(entries) == 0 {
				fmt.Println("(no upgrade history)")
				return nil
			}
			fmt.Printf("%-20s  %-22s  %-12s  %-10s → %-10s  %s\n", "AT", "RELEASE_ID", "STEP", "FROM", "TO", "DETAIL")
			for _, e := range entries {
				detail := e.Detail
				if e.Error != "" {
					detail = "ERR: " + e.Error
				}
				fmt.Printf("%-20s  %-22s  %-12s  %-10s → %-10s  %s\n",
					e.At.Format("2006-01-02 15:04:05"),
					truncate(e.ReleaseID, 22),
					truncate(e.Step, 12),
					truncate(e.FromSHA, 10),
					truncate(e.ToSHA, 10),
					truncate(detail, 60),
				)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit raw entries as JSON instead of a table")
	cmd.Flags().IntVar(&limit, "limit", 0, "Show only the last N entries (0 = all)")
	return cmd
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return strings.Repeat(".", n)
	}
	return s[:n-1] + "…"
}
