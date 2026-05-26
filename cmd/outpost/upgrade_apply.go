package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type applyPendingOut struct {
	OK        bool   `json:"ok"`
	Status    string `json:"status,omitempty"`
	Detail    string `json:"detail,omitempty"`
	ReleaseID string `json:"release_id,omitempty"`
	Commit    string `json:"commit,omitempty"`
}

// upgradeApplyCmd consumes a manual-mode host's queued envelope. The
// daemon stored it at <cacheDir>/outpost/upgrade.pending.json when
// the original /admin/upgrade POST landed; this verb just asks the
// daemon to re-run the envelope through the worker with Force=true,
// bypassing the manual gate.
//
// Cloudbox's "Apply" UI button does the same thing — but from the
// other direction: it re-POSTs the envelope to /admin/upgrade with
// Force=true. The two paths converge on the same Worker.Apply call,
// so the persisted file gets deleted either way on success.
func upgradeApplyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply the queued cloudbox-pushed upgrade (manual mode only)",
		Long: `On a host with update_mode=manual, /admin/upgrade pushes from cloudbox
persist their envelope to <cacheDir>/outpost/upgrade.pending.json
without applying. This verb runs the queued envelope through the
worker now — same stage / probe / swap / restart flow an auto-mode
host runs on receipt.

Refuses (exits non-zero) when no envelope is queued, or when the
host's update_mode is "never".`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out applyPendingOut
			if err := session.callTool(cmd.Context(), "outpost_apply_pending", map[string]any{}, &out); err != nil {
				if strings.Contains(err.Error(), "unknown tool") {
					return fmt.Errorf("apply is only available on paired hosts")
				}
				return err
			}
			switch out.Status {
			case "no_pending":
				return fmt.Errorf("no upgrade.pending.json on disk — nothing queued")
			case "disabled":
				return fmt.Errorf("update_mode is 'never' — flip it to apply this envelope")
			case "in_flight":
				return fmt.Errorf("another upgrade is in flight; wait then retry")
			case "same_commit":
				fmt.Println("already at the queued commit — no-op")
				return nil
			case "min_from":
				return fmt.Errorf("min_from precondition failed: %s", out.Detail)
			}
			fmt.Printf("apply: release=%s commit=%s status=%s\n", out.ReleaseID, out.Commit, out.Status)
			fmt.Println("upgrade scheduled — poll `outpost status` until configured returns.")
			return nil
		},
	}
	return cmd
}
