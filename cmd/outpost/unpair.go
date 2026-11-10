package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/portal"
)

// outpost unpair — forget every pairing this host holds. A cloudbox
// identity (pair-once, `cloudbox` in agent.json) is deleted at the master
// first via DELETE /api/v1/hosts/self; then the identity, the satellite
// attachments and the tunnel pairing (AgentName, Token, AccessToken) are
// cleared. Apps + outbound mounts + builtin toggles are preserved. Goes
// through the running daemon (MCP) so it restarts to drop the tunnel; with
// no daemon running it edits agent.json directly through the same admincore
// method.
func unpairCmd() *cobra.Command {
	var yes, dryRun, asJSON bool
	cmd := &cobra.Command{
		Use:     "unpair",
		Aliases: []string{"logout"},
		Short:   "Unpair this host: delete its cloudbox identity, detach satellites, clear the tunnel pairing.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dryRun {
				return printUnpairPlan(asJSON)
			}
			if !yes {
				fmt.Println("This deletes this host's cloudbox identity at the master (if paired), clears satellite")
				fmt.Println("attachments and AgentName/Token/AccessToken from agent.json, and restarts outpost.")
				fmt.Println("Apps, outbound mounts, and built-in toggles are preserved.")
				fmt.Println("Re-run with --yes to confirm (--dry-run shows the request).")
				return nil
			}
			if _, running := runningDaemonPID(); running {
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
			} else {
				core, err := offlineCore()
				if err != nil {
					return err
				}
				if _, err := core.Unpair(cmd.Context()); err != nil {
					return err
				}
			}
			// The user service is the switch that keeps a paired outpost alive.
			// Removing it also stops its supervisor, returning the installation to
			// the dormant state. Platform uninstallers are deliberately idempotent.
			if err := deactivatePairedService(); err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true})
			}
			fmt.Println("Unpaired. The per-user outpost service has been stopped and removed (dormant).")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what unpair would delete and clear; send nothing")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output")
	return cmd
}

func printUnpairPlan(asJSON bool) error {
	path := resolvePairingConfigPath("")
	fc, _ := conf.LoadFile(path)
	if fc == nil {
		fc = &conf.FileConfig{}
	}
	plan := map[string]any{"dry_run": true, "config": path}
	if fc.CloudboxPaired() {
		plan["request"] = "DELETE " + portal.HostSelfURL(fc.Cloudbox.PortalURL)
		plan["host"] = fc.Cloudbox.Host
	}
	plan["clears"] = []string{"cloudbox", "attachments", "agent_name", "portal_url", "server_addr", "server_port", "protocol", "token", "remote_port", "access_token", "auth_url", "client_only"}
	plan["attachments"] = len(fc.Attachments)
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(plan)
	}
	if r, ok := plan["request"]; ok {
		fmt.Printf("would send: %s (host %q)\n", r, fc.Cloudbox.Host)
	} else {
		fmt.Println("no cloudbox identity: nothing to delete at the master")
	}
	fmt.Printf("would clear in %s: %v\n", path, plan["clears"])
	return nil
}
