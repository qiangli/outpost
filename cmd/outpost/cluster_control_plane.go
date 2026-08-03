package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/vkcred"
)

// controlPlaneOut mirrors admincore.ControlPlaneResult over MCP.
type controlPlaneOut struct {
	ControlPlane     bool   `json:"control_plane"`
	BindAddr         string `json:"bind_addr"`
	BindPort         int    `json:"bind_port"`
	HasToken         bool   `json:"has_token"`
	TunnelToken      string `json:"tunnel_token,omitempty"`
	STCPSecret       string `json:"stcp_secret,omitempty"`
	APIAddr          string `json:"api_addr,omitempty"`
	LANExposed       bool   `json:"lan_exposed"`
	RestartPending   bool   `json:"restart_pending"`
	WorkerRejoinHint string `json:"worker_rejoin_hint,omitempty"`
}

// clusterControlPlaneCmd is the operator surface for control-plane PLACEMENT:
// whether this host runs the apiserver other machines join, as opposed to
// joining one itself (dhnt/docs/dks-control-plane-on-sphere.md).
//
//	outpost cluster control-plane                 # config
//	outpost cluster control-plane status          # health + node readiness
//	outpost cluster control-plane on              # host the apiserver here
//	outpost cluster control-plane on --bind-addr 0.0.0.0
//	outpost cluster control-plane off
//	outpost cluster control-plane token           # the value workers need
//	outpost cluster control-plane token rotate    # revoke + reissue
func clusterControlPlaneCmd() *cobra.Command {
	var bindAddr string
	var bindPort int

	cmd := &cobra.Command{
		Use:   "control-plane [on|off|status]",
		Short: "Host the DKS apiserver on this machine, or report whether it does",
		Long: `Report or set whether this host runs the DKS control plane.

Placement is a choice, not three architectures: the apiserver can live on
cloudbox, on a rented always-on box, or on this machine. Workers dial a
tunnel server the same way in every case, so switching placement is a
configuration change rather than a migration.

Enabling mints the join token if this host does not have one. Read it with
` + "`outpost cluster control-plane token`" + ` and configure workers with it —
it is this cluster's equivalent of the k3s node-token.

The tunnel binds 127.0.0.1 by default, on the assumption that workers reach
it over the mesh. Pass --bind-addr 0.0.0.0 to accept joins directly from the
network.`,
		ValidArgs: []string{"on", "off", "status"},
		Args:      cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()

			addrChanged := cmd.Flags().Changed("bind-addr")
			portChanged := cmd.Flags().Changed("bind-port")

			// No verb and no flags is a pure read — never a write, so
			// running it to check state cannot change state.
			if len(args) == 0 && !addrChanged && !portChanged {
				var out controlPlaneOut
				if err := session.callTool(cmd.Context(), "outpost_control_plane", map[string]any{}, &out); err != nil {
					return err
				}
				printControlPlane(out)
				return nil
			}

			params := map[string]any{}
			if len(args) == 1 {
				params["enabled"] = args[0] == "on"
			}
			if addrChanged {
				params["bind_addr"] = bindAddr
			}
			if portChanged {
				params["bind_port"] = bindPort
			}

			var out controlPlaneOut
			if err := session.callTool(cmd.Context(), "outpost_set_control_plane", params, &out); err != nil {
				return err
			}
			printControlPlane(out)
			if out.RestartPending {
				fmt.Println("\nRestarting outpost to apply.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&bindAddr, "bind-addr", "", "IP the tunnel server binds (default 127.0.0.1)")
	cmd.Flags().IntVar(&bindPort, "bind-port", 0, "Port the tunnel server binds (default 7000)")
	cmd.AddCommand(clusterControlPlaneTokenCmd())
	cmd.AddCommand(clusterControlPlaneStatusCmd())
	cmd.AddCommand(clusterControlPlaneVKCredentialCmd())
	return cmd
}

// clusterControlPlaneVKCredentialCmd mints the least-privilege virtual-kubelet
// bundle — the fourth join value, needed only by workers that select a virtual
// runtime. It lives beside `token` because the two are read in the same
// operator motion: gather everything on the host, carry it to the worker.
func clusterControlPlaneVKCredentialCmd() *cobra.Command {
	var namespaces []string
	var quiet bool
	cmd := &cobra.Command{
		Use:   "vk-credential",
		Short: "Mint the least-privilege vk credential bundle workers pass as --vk-bundle",
		Long: `Mint (idempotently) the virtual-kubelet credential of the control plane
hosted here, and print it as one opaque bundle for the worker's
` + "`outpost cluster join --vk-bundle`" + `.

The bundle carries the plane's CA, a ServiceAccount bearer token scoped to
exactly the verbs a virtual-kubelet node uses (register its Node, watch and
report its pods, read configmaps/secrets/services, emit events — nothing
else), and the namespace allow-list the worker enforces FAIL-CLOSED: pods
outside it are refused. --namespace names that list and provisions each
namespace on the plane; omitting it allows only "default".

The plane's admin kubeconfig is read locally to mint this and is never part
of the output — it does not leave this machine.

Re-minting returns the same token, so a second worker does not invalidate the
first. To rotate: kubectl -n ` + vkcred.SystemNamespace + ` delete secret ` + vkcred.TokenSecretName + `,
re-mint, re-join every vk worker.

Use --quiet to print the bare bundle for piping.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			args := map[string]any{}
			if len(namespaces) > 0 {
				args["namespaces"] = namespaces
			}
			var out struct {
				OK         bool     `json:"ok"`
				Bundle     string   `json:"bundle"`
				Namespaces []string `json:"namespaces"`
				Endpoint   string   `json:"endpoint,omitempty"`
			}
			if err := session.callTool(cmd.Context(), "outpost_control_plane_vk_credential", args, &out); err != nil {
				return err
			}
			if quiet {
				fmt.Println(out.Bundle)
				return nil
			}
			fmt.Printf("vk bundle:  %s\n", out.Bundle)
			fmt.Printf("namespaces: %s\n", strings.Join(out.Namespaces, ", "))
			fmt.Println("\nOn the worker, join with all four values:")
			fmt.Printf("  outpost cluster join %s --token … --stcp-secret … --node-token … \\\n", out.Endpoint)
			fmt.Println("      --vk-bundle " + out.Bundle[:min(24, len(out.Bundle))] + "… --cluster-virtual vk-podman")
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&namespaces, "namespace", nil, "Workload namespace to allow + provision (repeatable; default: default)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Print only the bundle")
	return cmd
}

func clusterControlPlaneTokenCmd() *cobra.Command {
	var quiet bool
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Print the tunnel token workers use to join the control plane hosted here",
		Long: `Print this cluster's join token — the equivalent of the k3s node-token.

It is a credential. It is deliberately absent from ` + "`outpost status`" + ` and
from ` + "`outpost cluster control-plane`" + ` so reading cluster state never puts
it on screen.

Use --quiet to print the bare value for piping.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out controlPlaneOut
			if err := session.callTool(cmd.Context(), "outpost_control_plane_token", map[string]any{}, &out); err != nil {
				return err
			}
			if out.TunnelToken == "" {
				return fmt.Errorf("no tunnel token on this host — run `outpost cluster control-plane on` first")
			}
			if quiet {
				fmt.Println(out.TunnelToken)
				return nil
			}
			// Print the WHOLE set a worker needs. Handing over the token
			// alone produces a worker that authenticates and then cannot
			// reach the apiserver — a failure that looks like a network
			// problem rather than a missing credential.
			fmt.Printf("endpoint:    %s\n", net.JoinHostPort(out.BindAddr, strconv.Itoa(out.BindPort)))
			fmt.Printf("token:       %s\n", out.TunnelToken)
			fmt.Printf("stcp secret: %s\n", out.STCPSecret)
			fmt.Printf("apiserver:   %s (on this host)\n", out.APIAddr)
			if !out.ControlPlane {
				fmt.Println("\nNote: this host is NOT currently hosting the apiserver, so nothing is listening.")
				fmt.Println("Run `outpost cluster control-plane on` to start it.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Print only the token")
	cmd.AddCommand(clusterControlPlaneRotateCmd())
	return cmd
}

func clusterControlPlaneRotateCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Mint a new tunnel token, invalidating every worker's configuration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Confirmation is not ceremony here: rotating breaks every worker
			// on its next reconnect, and the breakage surfaces on the WORKERS,
			// far from whoever ran this.
			if !yes {
				fmt.Println("Rotating invalidates the current token.")
				fmt.Println("Every worker joined to this control plane must be reconfigured,")
				fmt.Println("and they will fail on their next reconnect until you do.")
				fmt.Println("\nRe-run with --yes to confirm.")
				return nil
			}
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out controlPlaneOut
			if err := session.callTool(cmd.Context(), "outpost_rotate_control_plane_token", map[string]any{}, &out); err != nil {
				return err
			}
			fmt.Printf("token:    %s\n", out.TunnelToken)
			fmt.Printf("endpoint: %s\n", net.JoinHostPort(out.BindAddr, strconv.Itoa(out.BindPort)))
			fmt.Println()
			if out.WorkerRejoinHint != "" {
				fmt.Println(out.WorkerRejoinHint)
			} else {
				fmt.Println("Reconfigure every worker with the new token.")
			}
			if out.RestartPending {
				fmt.Println("Restarting outpost to apply.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	return cmd
}

func printControlPlane(out controlPlaneOut) {
	state := "off"
	if out.ControlPlane {
		state = "on"
	}
	fmt.Printf("control plane: %s\n", state)
	fmt.Printf("tunnel bind:   %s\n", net.JoinHostPort(out.BindAddr, strconv.Itoa(out.BindPort)))
	fmt.Printf("join token:    %s\n", tokenPresence(out.HasToken))
	if out.LANExposed {
		// Worth stating plainly rather than leaving the operator to read it
		// off an address: this is the difference between "reachable over the
		// mesh" and "accepting cluster joins from the network".
		fmt.Println("\nNOTE: bound to a non-loopback address — reachable from the network.")
	}
}

// controlPlaneStatusResult decodes outpost_control_plane_status. Named rather
// than the anonymous struct it used to be: the shape is written twice (decode
// site and print site), and an anonymous type makes adding a field a
// two-place edit that compiles happily if you forget the second one.
type controlPlaneStatusResult struct {
	Hosted              bool `json:"hosted"`
	ContainerExists     bool `json:"container_exists"`
	ContainerRunning    bool `json:"container_running"`
	APIServerServing    bool `json:"apiserver_serving"`
	APIServerStatusCode int  `json:"apiserver_status_code"`
	Nodes               []struct {
		Name  string `json:"name"`
		Ready bool   `json:"ready"`
	} `json:"nodes"`
	NodeCount     int    `json:"node_count"`
	JoinEndpoint  string `json:"join_endpoint"`
	HasJoinToken  bool   `json:"has_join_token"`
	HasNodeToken  bool   `json:"has_node_token"`
	HasSTCPSecret bool   `json:"has_stcp_secret"`

	NodeAddrReconcilerRunning bool   `json:"nodeaddr_reconciler_running"`
	NodeAddrLastRunAt         int64  `json:"nodeaddr_last_run_at"`
	NodeAddrLastError         string `json:"nodeaddr_last_error"`

	CheckedAt int64 `json:"checked_at"`
}

func printControlPlaneStatus(status controlPlaneStatusResult) {
	if !status.Hosted {
		fmt.Println("hosted: no")
		return
	}
	fmt.Println("hosted: yes")
	fmt.Printf("container exists:    %v\n", status.ContainerExists)
	fmt.Printf("container running:   %v\n", status.ContainerRunning)
	fmt.Printf("apiserver serving:   %v\n", status.APIServerServing)
	if status.APIServerStatusCode > 0 {
		fmt.Printf("apiserver status:    %d\n", status.APIServerStatusCode)
	}
	if status.JoinEndpoint != "" {
		fmt.Printf("join endpoint:       %s\n", status.JoinEndpoint)
	}
	fmt.Printf("credentials:         join_token=%v node_token=%v stcp_secret=%v\n",
		status.HasJoinToken, status.HasNodeToken, status.HasSTCPSecret)

	// The address reconciler. Called out on its own line because its absence
	// is invisible everywhere else: nodes report Ready and pods schedule while
	// every kubectl logs/exec/port-forward/top fails, since no node carries
	// the ExternalIP the apiserver dials kubelets through.
	switch {
	case !status.NodeAddrReconcilerRunning:
		fmt.Println("node addressing:     NOT RUNNING — the apiserver cannot reach kubelets")
		fmt.Println("                     (kubectl logs/exec/top will fail; see docs/cluster-peer.md)")
	case status.NodeAddrLastError != "":
		fmt.Printf("node addressing:     running, LAST PASS FAILED: %s\n", status.NodeAddrLastError)
	default:
		fmt.Println("node addressing:     running")
	}

	fmt.Printf("node count:          %d\n", status.NodeCount)
	if len(status.Nodes) > 0 {
		fmt.Println("nodes:")
		for _, n := range status.Nodes {
			readyStr := "ready"
			if !n.Ready {
				readyStr = "not ready"
			}
			fmt.Printf("  %s: %s\n", n.Name, readyStr)
		}
	}
}

func clusterControlPlaneStatusCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report the health and readiness of the hosted control plane",
		Long: `Show the hosted control plane's health: container state, apiserver serving,
node addressing reconciler liveness, and cluster node list with readiness status.

Does not reveal credential values.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var status controlPlaneStatusResult
			if err := session.callTool(cmd.Context(), "outpost_control_plane_status", map[string]any{}, &status); err != nil {
				return err
			}
			// --json exists for the "kubectl top nodes fails" runbook in
			// docs/cluster-peer.md, which needs nodeaddr_reconciler_running as a
			// value a script can branch on rather than a line to grep. Same flag
			// name and meaning as `outpost status --json`. It re-encodes the
			// decoded struct, so it inherits that type's redaction: fields the
			// CLI has no member for — including any token the daemon might ever
			// add — are dropped at decode and cannot reappear here.
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(status)
			}
			printControlPlaneStatus(status)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	return cmd
}

func tokenPresence(has bool) string {
	if has {
		return "set (outpost cluster control-plane token)"
	}
	return "none"
}
