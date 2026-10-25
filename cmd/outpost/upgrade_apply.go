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

// upgradeApplyCmd consumes a queued envelope left at
// <cacheDir>/outpost/upgrade.pending.json (written by hosts running a
// build that still queued under the removed manual update-mode). This
// verb asks the daemon to run the envelope through the worker now —
// same stage / probe / swap / restart flow a push runs on receipt.
//
// Cloudbox's "Apply" UI button does the same thing — but from the
// other direction: it re-POSTs the envelope to /admin/upgrade. The two
// paths converge on the same Worker.Apply call, so the persisted file
// gets deleted either way on success.
func upgradeApplyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply a queued cloudbox-pushed upgrade left in upgrade.pending.json",
		Long: `A queued envelope at <cacheDir>/outpost/upgrade.pending.json —
left behind by a build that still queued under the removed manual
update-mode — is run through the worker now: the same stage / probe /
swap / restart flow a push runs on receipt.

Refuses (exits non-zero) when no envelope is queued.`,
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
				return fmt.Errorf("apply refused: %s", out.Detail)
			case "in_flight":
				return fmt.Errorf("another upgrade is in flight; wait then retry")
			case "same_commit":
				fmt.Println("already at the queued commit — no-op")
				return nil
			case "min_from":
				return fmt.Errorf("min_from precondition failed: %s", out.Detail)
			case "downgrade":
				return fmt.Errorf("refused: %s", out.Detail)
			}
			fmt.Printf("apply: release=%s commit=%s status=%s\n", out.ReleaseID, out.Commit, out.Status)
			fmt.Println("upgrade scheduled — poll `outpost status` until configured returns.")
			return nil
		},
	}
	return cmd
}
