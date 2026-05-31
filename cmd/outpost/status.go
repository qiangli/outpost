package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// outpost status — one-page summary of pairing + builtins + outbound.
// Reads the outpost://status and outpost://config resources; renders
// either a human table or JSON.
func statusCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show outpost pairing, built-ins, and outbound state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
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
				}, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			fmt.Println("Pairing")
			if status.Configured {
				fmt.Printf("  agent_name  %s\n", status.AgentName)
				fmt.Printf("  cloudbox    %s\n", status.CloudboxURL)
				fmt.Printf("  protocol    %s\n", cfg.Protocol)
				fmt.Printf("  has_token   %t\n", cfg.HasToken)
			} else {
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
			row("podman", cfg.Podman.Enabled)
			row("ollama", cfg.Ollama.Enabled)
			row("ollama_pool", cfg.OllamaPoolEnabled)
			row("otel", cfg.OtelEnabled)
			row("otel_pool", cfg.OtelPoolEnabled)
			row("cluster", cfg.Cluster.Enabled)
			fmt.Printf("  %-22s  %s\n", "update_mode", cfg.UpdateMode)
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
	return cmd
}
