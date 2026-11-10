package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// outpost satellite {list,attach,detach} — attach this host to a satellite
// with its ONE cloudbox pairing (no satellite pairing code), yoke
// docs/cloudbox-members-attach-wire.md §4. Goes through the running daemon
// (MCP) so attach/detach restart it onto/off the satellite tunnel; with no
// daemon running it edits agent.json through the same admincore methods.
// Agents reach it as `bashy outpost satellite ...`.
func satelliteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "satellite",
		Short: "Attach this host to a satellite of its account (list, attach, detach).",
	}
	cmd.AddCommand(satelliteListCmd(), satelliteAttachCmd(), satelliteDetachCmd())
	return cmd
}

// satelliteCall runs one satellite operation against the daemon when it is
// running, else against a one-shot offline core.
func satelliteCall(ctx context.Context, tool string, args map[string]any, out any, offline func(*admincore.Server) (any, error)) error {
	if _, running := runningDaemonPID(); running {
		session, err := dialMCP(ctx)
		if err != nil {
			return err
		}
		defer session.close()
		return session.callTool(ctx, tool, args, out)
	}
	core, err := offlineCore()
	if err != nil {
		return err
	}
	res, err := offline(core)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(res)
	return json.Unmarshal(b, out)
}

func satelliteListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the satellites this host may attach to, with attached flags.",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out admincore.SatelliteListResult
			err := satelliteCall(cmd.Context(), "outpost_satellite_list", map[string]any{}, &out,
				func(c *admincore.Server) (any, error) { return c.SatelliteList(cmd.Context()) })
			if err != nil {
				return err
			}
			if out.Satellites == nil {
				out.Satellites = []admincore.SatelliteRow{}
			}
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(out)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tSTATE\tATTACHED\tENDPOINT")
			for _, s := range out.Satellites {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\n", s.ID, s.Name, s.State, s.Attached, s.Endpoint)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output")
	return cmd
}

func satelliteAttachCmd() *cobra.Command {
	var asJSON, dryRun, migrate bool
	cmd := &cobra.Command{
		Use:   "attach ID|NAME",
		Short: "Attach this host to a satellite with its cloudbox pairing (no pairing code).",
		Long: `Attach this host to one satellite of its account: the master issues an
attach token for the host token in agent.json "cloudbox", the satellite
redeems it for a satellite host token + tunnel registration, stored as
attachments[0]. The daemon restarts and tunnels to the satellite, publishing
the local apps. One attachment per host: a second satellite is refused with
409 attached_elsewhere until you detach.

--migrate replaces a legacy satellite pairing (top-level agent_name /
access_token from a pairing code) with the attachment.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := admincore.SatelliteAttachParams{Satellite: args[0], Migrate: migrate, DryRun: dryRun}
			var out admincore.SatelliteResult
			err := satelliteCall(cmd.Context(), "outpost_satellite_attach",
				map[string]any{"satellite": p.Satellite, "migrate": p.Migrate, "dry_run": p.DryRun}, &out,
				func(c *admincore.Server) (any, error) { return c.SatelliteAttach(cmd.Context(), p) })
			if err != nil {
				return err
			}
			return printSatelliteResult(out, asJSON, "attached to")
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the requests attach would send; send nothing")
	cmd.Flags().BoolVar(&migrate, "migrate", false, "Replace a legacy satellite pairing with the attachment")
	return cmd
}

func satelliteDetachCmd() *cobra.Command {
	var asJSON, dryRun bool
	cmd := &cobra.Command{
		Use:   "detach [ID|NAME]",
		Short: "Detach this host from a satellite and stop its tunnel.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := admincore.SatelliteDetachParams{DryRun: dryRun}
			if len(args) == 1 {
				p.Satellite = args[0]
			}
			var out admincore.SatelliteResult
			err := satelliteCall(cmd.Context(), "outpost_satellite_detach",
				map[string]any{"satellite": p.Satellite, "dry_run": p.DryRun}, &out,
				func(c *admincore.Server) (any, error) { return c.SatelliteDetach(cmd.Context(), p) })
			if err != nil {
				return err
			}
			return printSatelliteResult(out, asJSON, "detached from")
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the requests detach would send; send nothing")
	return cmd
}

func printSatelliteResult(out admincore.SatelliteResult, asJSON bool, verb string) error {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}
	for _, w := range out.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if out.DryRun {
		for _, r := range out.Requests {
			fmt.Println("would send:", r)
		}
		return nil
	}
	name := out.SatelliteID
	if out.Name != "" {
		name = fmt.Sprintf("%s (%s)", out.Name, out.SatelliteID)
	}
	fmt.Printf("%s %s as %q\n", verb, name, out.Host)
	if out.Migrated {
		fmt.Println("the legacy satellite pairing was replaced by the attachment")
	}
	if out.RestartPending {
		fmt.Println("the daemon restarts to apply the tunnel change")
	}
	return nil
}
