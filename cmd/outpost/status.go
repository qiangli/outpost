package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// outpost status — one-page summary of pairing + builtins + outbound.
// Reads the outpost://status and outpost://config resources; renders
// either a human table or JSON.
func statusCmd() *cobra.Command {
	var jsonOut, localPresence bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show outpost pairing, built-ins, and outbound state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if localPresence {
				running, addr, err := localDaemonPresence()
				if err != nil {
					return err
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"running": running, "addr": addr})
			}
			session, err := dialMCP(cmd.Context())
			if err != nil {
				// A dormant install has no daemon to answer MCP. Its config is
				// still enough to report the pairing state without starting it.
				path, pathErr := conf.DefaultConfigPath()
				if pathErr != nil {
					return err
				}
				fc, loadErr := conf.LoadFile(path)
				if loadErr != nil {
					return err
				}
				if fc == nil {
					fc = &conf.FileConfig{}
				}
				if jsonOut {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"state": pairingState(fc.AgentName), "agent_name": fc.AgentName})
				}
				fmt.Println(pairingState(fc.AgentName))
				return nil
			}
			defer session.close()
			var status admincore.StatusView
			if err := session.readResource(cmd.Context(), "outpost://status", &status); err != nil {
				return err
			}
			var cfg admincore.SafeView
			if err := session.readResource(cmd.Context(), "outpost://config", &cfg); err != nil {
				return err
			}
			if jsonOut {
				b, _ := json.MarshalIndent(map[string]any{
					"status": status,
					"config": cfg,
					"state":  pairingState(status.AgentName),
				}, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			fmt.Println("Pairing")
			if status.Configured {
				fmt.Printf("  state       active (paired as %s)\n", status.AgentName)
				fmt.Printf("  agent_name  %s\n", status.AgentName)
				fmt.Printf("  cloudbox    %s\n", status.CloudboxURL)
				fmt.Printf("  protocol    %s\n", cfg.Protocol)
				fmt.Printf("  has_token   %t\n", cfg.HasToken)
			} else {
				fmt.Println("  state       dormant (unpaired)")
				fmt.Println("  unpaired — run `outpost register` or visit the admin UI to pair.")
			}
			fmt.Printf("  os_user     %s\n", status.CurrentOSUser)
			fmt.Println()
			fmt.Println("Build")
			fmt.Printf("  version     %s\n", status.Build.Short())
			if status.Build.VCSTime != "" {
				fmt.Printf("  vcs_time    %s\n", status.Build.VCSTime)
			}
			if status.Build.GoVersion != "" {
				fmt.Printf("  go          %s\n", status.Build.GoVersion)
			}
			if status.BinaryPath != "" {
				fmt.Printf("  binary      %s\n", status.BinaryPath)
			}
			if status.Build.BinarySize > 0 {
				fmt.Printf("  size        %.1f MB\n", float64(status.Build.BinarySize)/(1024*1024))
			}
			if !status.Build.InstalledAt.IsZero() {
				fmt.Printf("  installed   %s\n", status.Build.InstalledAt.Local().Format("2006-01-02 15:04:05"))
			}
			if !status.Build.DaemonStartedAt.IsZero() {
				fmt.Printf("  running     %s\n", status.Build.DaemonStartedAt.Local().Format("2006-01-02 15:04:05"))
			}
			if status.Build.OSVersion != "" {
				fmt.Printf("  os          %s (%s/%s)\n", status.Build.OSVersion, status.Build.OS, status.Build.Arch)
			}
			fmt.Println()
			fmt.Println("Built-ins")
			row := func(name string, on bool) { fmt.Printf("  %-22s  %t\n", name, on) }
			row("shell", cfg.ShellEnabled)
			row("desktop", cfg.DesktopEnabled)
			row("clipboard", cfg.ClipboardEnabled)
			row("ssh", cfg.SSHEnabled)
			row("ssh_allow_local_fwd", cfg.SSHAllowLocalForward)
			row("sftp", cfg.SFTPEnabled)
			row("mesh", cfg.MeshEnabled)
			row("cluster", cfg.Cluster.Enabled)
			printClusterStatus(os.Stdout, status)
			// The podman proxy, the container sandbox, the Ollama proxy and
			// its pool, the warm and shard planes, the LAN inference
			// listener, the File Browser and the observability / ycode
			// surfaces are service units — `outpost services` is where
			// their desired state and live state are reported together.
			if len(cfg.Services) > 0 {
				fmt.Println()
				fmt.Println("Service units (desired state; `outpost services` for live state)")
				for _, u := range cfg.Services {
					fmt.Printf("  %-22s  %-5t  %s\n", u.Name, u.Enabled, u.Provider)
				}
			}
			fmt.Println()
			fmt.Println("Apps")
			if len(cfg.Apps) == 0 {
				fmt.Println("  (none)")
			} else {
				for _, a := range cfg.Apps {
					fmt.Printf("  %-20s  %-8s  enabled=%t\n", a.Name, a.Scheme, a.Enabled)
				}
			}
			fmt.Println()
			fmt.Println("Outbound")
			if len(cfg.Outbound) == 0 {
				fmt.Println("  (none)")
			} else {
				for _, o := range cfg.Outbound {
					fmt.Printf("  %-20s  scheme=%s  connected=%t\n", o.Path, o.Scheme, o.Connected)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	cmd.Flags().BoolVar(&localPresence, "local-presence", false, "Report local daemon presence without authentication (installer probe)")
	return cmd
}

func pairingState(name string) string {
	if name == "" {
		return "dormant (unpaired)"
	}
	return fmt.Sprintf("active (paired as %s)", name)
}

// printClusterStatus renders the control-plane failure and the worker's view
// of the plane it joined.
func printClusterStatus(w io.Writer, status admincore.StatusView) {
	if status.ControlPlaneState == "failed" {
		fmt.Fprintf(w, "  %-22s  FAILED: %s\n", "control_plane", status.ControlPlaneFailure)
	}
	c := status.Cluster
	if c == nil {
		return
	}
	fmt.Fprintf(w, "  %-22s  joined endpoint=%s apiserver_reachable=%s\n", "cluster", c.Endpoint, c.APIReachable)
	if !c.LastSuccess.IsZero() {
		fmt.Fprintf(w, "  %-22s  %s\n", "cluster last_success", c.LastSuccess.Local().Format("2006-01-02 15:04:05"))
	}
	if c.LastError != "" {
		fmt.Fprintf(w, "  %-22s  %s\n", "cluster last_error", c.LastError)
	}
}
