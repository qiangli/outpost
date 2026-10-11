package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// servicesCmd is outpost's READ surface over the host's service units: what
// is declared, what the operator asked for, and what the monitor reports.
//
// Deliberately read-only, and deliberately not a second `service` command
// tree. Unit definitions and their verbs belong to `bashy service`
// (add/show/set/rm/enable/disable/start/stop/restart/status/logs); this is
// the view from the side that publishes them, so an operator can see in one
// place whether a unit is up AND whether outpost has it on the tunnel.
// Desired state is written with `outpost builtins set --service NAME=on|off`
// or, equivalently, `bashy service enable NAME`.
func servicesCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:     "services",
		Aliases: []string{"units"},
		Short:   "Show the host's service units: desired state, monitor state, and what outpost publishes",
		Long: `Show every service unit this host declares.

Units are YAML declarations in the registry rings (embedded built-in seeds,
shared dirs, host-local overrides). ` + "`bashy supervisord`" + ` — the service
monitor — owns their processes; the outpost daemon publishes each unit's
declared app route and mesh names while the unit is active and withdraws them
when it is not.

STATE is the monitor's: inactive, starting, active, degraded, failed or
stopping. A dash means the monitor could not be reached, which the footer
says out loud rather than presenting stale rows as live truth.

  outpost builtins set --service meet=on     # desired state (applies live)
  bashy service show meet --yaml             # the declaration
  bashy service logs meet -f                 # the unit's own log`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var view admincore.ServiceStatusView
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			if err := session.readResource(cmd.Context(), "outpost://services", &view); err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(view)
			}
			if len(view.Units) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No service units are declared. `bashy service list` to check the registry.")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "UNIT\tPROVIDER\tWANTED\tSTATE\tPORTS\tPUBLISHED\tRING")
			for _, u := range view.Units {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					u.Name, u.Provider, enabledWord(u.Enabled), dashIfEmpty(u.State),
					dashIfEmpty(strings.Join(u.Ports, ",")), dashIfEmpty(strings.Join(u.Publishing, ",")),
					dashIfEmpty(u.Ring))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			for _, u := range view.Units {
				if u.Reason != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s\n", u.Name, u.Reason)
				}
			}
			if !view.MonitorReachable {
				fmt.Fprintln(cmd.OutOrStdout(),
					"\nThe service monitor did not answer; STATE is the last value seen.\n"+
						"`outpost supervisord` keeps `bashy supervisord` running — check its log, or run the monitor by hand with `bashy supervisord`.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	return cmd
}

func enabledWord(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
