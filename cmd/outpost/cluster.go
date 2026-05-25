package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
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
	cmd.AddCommand(clusterKubeconfigCmd(), clusterSetCmd(), clusterClearCmd())
	return cmd
}

// clusterSetCmd persists a pasted kubeconfig into agent.json and
// optionally flips the join switch. Mirrors POST /api/cluster/kubeconfig
// in the admin UI and the outpost_set_kubeconfig MCP tool.
func clusterSetCmd() *cobra.Command {
	var (
		fileFlag   string
		nodeName   string
		enable     bool
		stdinFlag  bool
	)
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Persist a kubeconfig YAML (and optionally join the cluster)",
		Long: `Read a kubeconfig YAML from --file or stdin and persist its
apiserver URL + bearer token + CA into agent.json's Cluster section.
Pass --enable to also flip the join switch — the daemon restarts so
vkpodman picks up the new credentials.

Examples:
  outpost cluster set --file ./k3s.yaml --enable
  cat ~/.kube/config | outpost cluster set --stdin --node-name laptop --enable
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			yaml, err := readKubeconfigInput(fileFlag, stdinFlag)
			if err != nil {
				return err
			}
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			args := map[string]any{
				"kubeconfig": yaml,
				"enable":     enable,
			}
			if nodeName != "" {
				args["node_name"] = nodeName
			}
			var out struct {
				RestartPending bool `json:"restart_pending"`
			}
			if err := session.callTool(cmd.Context(), "outpost_set_kubeconfig", args, &out); err != nil {
				return err
			}
			if out.RestartPending {
				fmt.Println("Saved + joining. Restarting outpost — poll `outpost status` until configured=true.")
			} else {
				fmt.Println("Saved (cluster not yet enabled; pass --enable to join).")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&fileFlag, "file", "", "Path to a kubeconfig YAML file")
	cmd.Flags().BoolVar(&stdinFlag, "stdin", false, "Read the kubeconfig YAML from stdin")
	cmd.Flags().StringVar(&nodeName, "node-name", "", "Optional override; empty defaults to AgentName")
	cmd.Flags().BoolVar(&enable, "enable", false, "Also flip Cluster.Enabled (the daemon restarts to join)")
	return cmd
}

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

// readKubeconfigInput resolves --file vs --stdin; exactly one must
// be supplied. Returns the raw YAML bytes as a string.
func readKubeconfigInput(file string, stdinFlag bool) (string, error) {
	if file != "" && stdinFlag {
		return "", errors.New("--file and --stdin are mutually exclusive")
	}
	if file == "" && !stdinFlag {
		return "", errors.New("pass --file <path> or --stdin")
	}
	if stdinFlag {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(b), nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", file, err)
	}
	return string(b), nil
}

// clusterKubeconfigCmd prints a kubectl-ready YAML kubeconfig to
// stdout. It uses the persisted access_token to call cloudbox's
// /api/cluster/kubeconfig endpoint (the same one the agent boot path
// uses), then templates the response into the minimal three-stanza
// kubeconfig kubectl needs.
//
// Use:
//
//	outpost cluster kubeconfig > ~/.kube/outpost.yaml
//	export KUBECONFIG=~/.kube/outpost.yaml
//	kubectl get nodes
//
// --node defaults to the laptop's own paired name (fc.AgentName); pass
// a different host name to mint a kubeconfig scoped to that host's
// ServiceAccount instead (still requires the requesting account to
// own that host on cloudbox).
func clusterKubeconfigCmd() *cobra.Command {
	var nodeFlag string
	cmd := &cobra.Command{
		Use:   "kubeconfig",
		Short: "Print a YAML kubeconfig for the cloudbox cluster",
		Long: `Fetch a fresh per-host ServiceAccount token from cloudbox and
render it as a kubeconfig YAML on stdout. Reads the saved
access_token from agent.json (same file outpost register writes).

The resulting kubeconfig authenticates as the host's
ServiceAccount in the cloudbox outpost-nodes namespace; what RBAC
it has is determined cloudbox-side. Token lifetime is what
cloudbox mints (24h by default) — re-run the command for a fresh
one before it expires.`,
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
			parsed, err := vkpodman.FetchKubeconfig(cmd.Context(), cloudboxBase, fc.AccessToken, node)
			if err != nil {
				return fmt.Errorf("fetch kubeconfig: %w", err)
			}
			_, err = fmt.Fprint(os.Stdout, renderKubeconfigYAML(node, parsed))
			return err
		},
	}
	cmd.Flags().StringVar(&nodeFlag, "node", "",
		"Host name to mint the kubeconfig for (defaults to this machine's agent name)")
	return cmd
}

// renderKubeconfigYAML returns the minimal kubeconfig YAML kubectl
// needs: one cluster, one user, one context, current-context set. CA
// is inlined as certificate-authority-data when present; empty CA
// means trust the system roots, which is what cloudbox-fronted
// HTTPS deployments behind a real public cert want.
//
// The string is built by hand rather than going through sigs.k8s.io
// /yaml because the surface is tiny, the field-order matters for
// human readability, and not pulling another encoder dep into the
// CLI keeps the binary small.
func renderKubeconfigYAML(contextName string, p *vkpodman.ParsedKubeconfig) string {
	clusterName := "outpost-cluster"
	userName := "outpost-" + contextName
	var caField string
	if len(p.CA) > 0 {
		caField = "    certificate-authority-data: " + base64.StdEncoding.EncodeToString(p.CA) + "\n"
	}
	return "apiVersion: v1\n" +
		"kind: Config\n" +
		"clusters:\n" +
		"- name: " + clusterName + "\n" +
		"  cluster:\n" +
		"    server: " + p.APIURL + "\n" +
		caField +
		"users:\n" +
		"- name: " + userName + "\n" +
		"  user:\n" +
		"    token: " + p.Token + "\n" +
		"contexts:\n" +
		"- name: " + contextName + "\n" +
		"  context:\n" +
		"    cluster: " + clusterName + "\n" +
		"    user: " + userName + "\n" +
		"current-context: " + contextName + "\n"
}
