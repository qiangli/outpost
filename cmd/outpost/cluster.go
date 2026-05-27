package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/userkube"
	"github.com/qiangli/outpost/internal/agent/vkpodman"
)

// clusterCmd is the `outpost cluster <subcommand>` group. Currently
// holds one subcommand (kubeconfig) but structured as a group so
// future additions (e.g. `outpost cluster nodes`, `outpost cluster
// status`) slot in without redefining the surface.
func clusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Interact with the cloudbox virtual-podman cluster",
	}
	cmd.AddCommand(clusterKubeconfigCmd(), clusterClearCmd())
	return cmd
}

// `outpost cluster set` (bring-your-own paste path) is gone — outposts
// only join their owning cloudbox's cluster, which auto-fetches a
// kubeconfig on next boot once `outpost builtins set --cluster=on` is
// flipped. For a different cluster, pair another outpost against
// the cloudbox managing that cluster.

// clusterClearCmd wipes the persisted kubeconfig and disables the
// cluster toggle — i.e. leave the cluster. Mirrors DELETE
// /api/cluster/kubeconfig and outpost_clear_kubeconfig.
func clusterClearCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Wipe persisted kubeconfig and leave the cluster",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				fmt.Println("This will clear Cluster.{APIURL,Token,CA,Enabled} from agent.json.")
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
			if err := session.callTool(cmd.Context(), "outpost_clear_kubeconfig", map[string]any{}, &out); err != nil {
				return err
			}
			if out.RestartPending {
				fmt.Println("Cleared. Restarting outpost — vkpodman will stop on the next boot.")
			} else {
				fmt.Println("Cleared.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	return cmd
}

// clusterKubeconfigCmd writes a kubectl-ready YAML kubeconfig to
// disk by default (was stdout). Reads the persisted access_token,
// asks cloudbox for fresh credentials, renders the minimal four-
// stanza kubeconfig kubectl needs, and writes it to the canonical
// path ($OUTPOST_KUBECONFIG_PATH or $HOME/.kube/outpost.yaml).
//
// The daemon ALSO writes this file automatically when cluster.enabled
// flips on, plus on a refresh button in the admin UI — this CLI is
// kept around for headless flows / non-paired-host kubeconfig
// minting via --node.
//
// Use:
//
//	outpost cluster kubeconfig                  # writes ~/.kube/outpost.yaml
//	export KUBECONFIG=~/.kube/outpost.yaml      # one-time per shell
//	kubectl get nodes
//
//	outpost cluster kubeconfig --stdout > foo.yaml   # backward-compat shape
//	outpost cluster kubeconfig --output ~/foo.yaml   # custom path
//	outpost cluster kubeconfig --node other-host     # mint for a different host
func clusterKubeconfigCmd() *cobra.Command {
	var (
		nodeFlag   string
		outputFlag string
		stdoutFlag bool
	)
	cmd := &cobra.Command{
		Use:   "kubeconfig",
		Short: "Mint a kubectl-ready kubeconfig and write it to disk (or stdout)",
		Long: `Fetch a fresh per-host ServiceAccount token from cloudbox and
render it as a kubeconfig YAML. By default writes to
$HOME/.kube/outpost.yaml (or $OUTPOST_KUBECONFIG_PATH if set).
Pass --stdout to print instead, --output to write to a custom path.

The resulting kubeconfig authenticates as the host's ServiceAccount
in cloudbox's outpost-nodes namespace; what RBAC it has is
determined cloudbox-side. Token lifetime is what cloudbox mints
(24h by default) — re-run before expiry or click Refresh in the
admin UI's Cluster section.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, err := conf.DefaultConfigPath()
			if err != nil {
				return err
			}
			fc, err := conf.LoadFile(cfgPath)
			if err != nil {
				return fmt.Errorf("load %s: %w", cfgPath, err)
			}
			if fc == nil || fc.AccessToken == "" {
				return errors.New("no access_token saved — run `outpost register` first")
			}
			node := strings.TrimSpace(nodeFlag)
			if node == "" {
				node = fc.AgentName
			}
			if node == "" {
				return errors.New("no node name (--node) and no AgentName in saved config — pass --node explicitly")
			}
			cloudboxBase := cloudboxHTTPBase(fc)
			if cloudboxBase == "" {
				return errors.New("no cloudbox URL in saved config (server_addr / protocol missing)")
			}

			if stdoutFlag {
				parsed, err := vkpodman.FetchKubeconfig(cmd.Context(), cloudboxBase, fc.AccessToken, node)
				if err != nil {
					return fmt.Errorf("fetch kubeconfig: %w", err)
				}
				_, err = fmt.Fprint(os.Stdout, userkube.Render(node, parsed))
				return err
			}

			path, err := userkube.FetchAndWrite(cmd.Context(), cloudboxBase, fc.AccessToken, node, outputFlag)
			if err != nil {
				return err
			}
			fmt.Printf("wrote %s\n", path)
			fmt.Println("Use it: export KUBECONFIG=" + path)
			return nil
		},
	}
	cmd.Flags().StringVar(&nodeFlag, "node", "",
		"Host name to mint the kubeconfig for (defaults to this machine's agent name)")
	cmd.Flags().StringVar(&outputFlag, "output", "",
		"Path to write to (default $HOME/.kube/outpost.yaml or $OUTPOST_KUBECONFIG_PATH)")
	cmd.Flags().BoolVar(&stdoutFlag, "stdout", false,
		"Print to stdout instead of writing to a file (legacy behavior)")
	return cmd
}
