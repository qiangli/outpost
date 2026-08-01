package main

import (
	"fmt"
	"net"
	"strconv"

	"github.com/spf13/cobra"
)

// controlPlaneOut mirrors admincore.ControlPlaneResult over MCP.
type controlPlaneOut struct {
	ControlPlane   bool   `json:"control_plane"`
	BindAddr       string `json:"bind_addr"`
	BindPort       int    `json:"bind_port"`
	HasToken       bool   `json:"has_token"`
	TunnelToken    string `json:"tunnel_token,omitempty"`
	STCPSecret     string `json:"stcp_secret,omitempty"`
	APIAddr        string `json:"api_addr,omitempty"`
	LANExposed     bool   `json:"lan_exposed"`
	RestartPending bool   `json:"restart_pending"`
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
			fmt.Println("\nReconfigure every worker with the new token.")
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

func printControlPlaneStatus(status struct {
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
	CheckedAt     int64  `json:"checked_at"`
}) {
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
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report the health and readiness of the hosted control plane",
		Long: `Show the hosted control plane's health: container state, apiserver serving,
and cluster node list with readiness status.

Does not reveal credential values.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var status struct {
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
				CheckedAt     int64  `json:"checked_at"`
			}
			if err := session.callTool(cmd.Context(), "outpost_control_plane_status", map[string]any{}, &status); err != nil {
				return err
			}
			printControlPlaneStatus(status)
			return nil
		},
	}
	return cmd
}

func tokenPresence(has bool) string {
	if has {
		return "set (outpost cluster control-plane token)"
	}
	return "none"
}
