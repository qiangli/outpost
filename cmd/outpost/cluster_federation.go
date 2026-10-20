package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// clusterFederationCmd is the operator surface for Karmada federation on a
// host that hosts a DKS control plane.
//
//	outpost cluster federation              # status
//	outpost cluster federation on           # publish Karmada, install, self-register
//	outpost cluster federation off
//	outpost cluster federation token        # what a member needs to join
func clusterFederationCmd() *cobra.Command {
	var jsonOut, force bool
	var clusterName, kubeconfig string
	var apiPort int

	cmd := &cobra.Command{
		Use:   "federation [on|off|status|token]",
		Short: "Federate this host's DKS with Karmada, or report federation state",
		Long: `Report or change this host's Karmada federation role.

` + "`on`" + ` requires a hosted control plane (` + "`outpost cluster control-plane on`" + `). It
records intent and restarts; the daemon then installs the karmada built-in,
writes the federation kubeconfig, learns the CA hash that identifies the
federation (fed-<12hex>) and registers this host's own plane as a member.
Run ` + "`status`" + ` to follow progress — it never prints credentials.

` + "`token`" + ` mints a bootstrap token and prints what a member needs. Those are
credentials; use --quiet to pipe the bare bootstrap token.

Federation is not supported on Windows (karmadactl has no Windows release).`,
		ValidArgs: []string{"on", "off", "status", "token"},
		Args:      cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			verb := "status"
			if len(args) == 1 {
				verb = args[0]
			}
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			ctx := cmd.Context()

			switch verb {
			case "status":
				var out admincore.FederationResult
				if err := session.callTool(ctx, "outpost_federation", map[string]any{}, &out); err != nil {
					return err
				}
				return printFederation(out, jsonOut)
			case "on":
				params := map[string]any{}
				if clusterName != "" {
					params["cluster_name"] = clusterName
				}
				if apiPort != 0 {
					params["api_port"] = apiPort
				}
				if kubeconfig != "" {
					params["kubeconfig"] = kubeconfig
				}
				var out admincore.FederationResult
				if err := session.callTool(ctx, "outpost_federation_on", params, &out); err != nil {
					return err
				}
				if err := printFederation(out, jsonOut); err != nil {
					return err
				}
				if out.RestartPending && !jsonOut {
					fmt.Println("\nRestarting outpost to apply.")
				}
				return nil
			case "off":
				var out admincore.FederationResult
				if err := session.callTool(ctx, "outpost_federation_off", map[string]any{"force": force}, &out); err != nil {
					return err
				}
				if err := printFederation(out, jsonOut); err != nil {
					return err
				}
				if out.Warning != "" && !jsonOut {
					fmt.Fprintln(os.Stderr, "warning:", out.Warning)
				}
				return nil
			case "token":
				var out admincore.FederationTokenResult
				if err := session.callTool(ctx, "outpost_federation_token", map[string]any{}, &out); err != nil {
					return err
				}
				if quiet, _ := cmd.Flags().GetBool("quiet"); quiet {
					fmt.Println(out.BootstrapToken)
					return nil
				}
				if jsonOut {
					return json.NewEncoder(os.Stdout).Encode(out)
				}
				fmt.Printf("endpoint:         %s\n", out.Endpoint)
				fmt.Printf("tunnel token:     %s\n", out.TunnelToken)
				fmt.Printf("stcp secret:      %s\n", out.STCPSecret)
				fmt.Printf("bootstrap token:  %s\n", out.BootstrapToken)
				fmt.Printf("ca hash:          sha256:%s\n", out.CAHash)
				fmt.Printf("federation id:    %s\n\n%s\n", out.FederationID, out.JoinHint)
				return nil
			}
			return fmt.Errorf("unknown verb %q (want on|off|status|token)", verb)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print JSON")
	cmd.Flags().BoolVarP(new(bool), "quiet", "q", false, "token: print only the bootstrap token")
	cmd.Flags().BoolVar(&force, "force", false, "off: clear the config even if the karmada built-in cannot be uninstalled")
	cmd.Flags().StringVar(&clusterName, "cluster-name", "", "on: name this host's plane registers under (default: agent name)")
	cmd.Flags().IntVar(&apiPort, "api-port", 0, "on: loopback port of the federation apiserver forward (default 15443)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "on: path for the materialised Karmada kubeconfig")
	return cmd
}

func printFederation(out admincore.FederationResult, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	if !out.Enabled {
		fmt.Println("federation: off")
		return nil
	}
	fmt.Printf("federation:     %s\n", out.Role)
	fmt.Printf("karmada:        %s\n", orDash(out.Phase))
	if out.FederationID != "" {
		fmt.Printf("federation id:  %s\n", out.FederationID)
	}
	fmt.Printf("cluster name:   %s\n", orDash(out.ClusterName))
	fmt.Printf("api port:       %d\n", out.APIPort)
	fmt.Printf("kubeconfig:     %s (present: %v)\n", out.Kubeconfig, out.KubeconfigOK)
	fmt.Printf("registered:     %v\n", out.SelfRegistered)
	fmt.Printf("members:        %d\n", len(out.Members))
	for _, member := range out.Members {
		fmt.Printf("  %s  ready=%v  sync=%s\n", member.Name, member.Ready, orDash(member.SyncMode))
	}
	if out.LastError != "" {
		fmt.Printf("last error:     %s\n", out.LastError)
	}
	return nil
}
