package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// outpost builtins {show,set} — CLI mirror of the SPA's built-in app
// toggles. `set` accepts each switch as an --<name>=on/off flag; only
// flags actually passed are sent to the daemon (pointer-bool semantics).
func builtinsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "builtins",
		Short: "Show or toggle built-in routes (shell/desktop/clipboard/ssh/sftp/podman/ollama/cluster)",
	}
	cmd.AddCommand(builtinsShowCmd(), builtinsSetCmd())
	return cmd
}

func builtinsShowCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the current state of every built-in toggle",
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var view admincore.SafeView
			if err := session.readResource(cmd.Context(), "outpost://config", &view); err != nil {
				return err
			}
			if jsonOut {
				b, _ := json.MarshalIndent(view, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			row := func(name string, on bool) { fmt.Printf("  %-22s  %t\n", name, on) }
			fmt.Println("Built-ins:")
			row("shell", view.ShellEnabled)
			row("desktop", view.DesktopEnabled)
			row("clipboard", view.ClipboardEnabled)
			row("ssh", view.SSHEnabled)
			row("ssh_allow_local_fwd", view.SSHAllowLocalForward)
			row("sftp", view.SFTPEnabled)
			row("podman", view.Podman.Enabled)
			row("ollama", view.Ollama.Enabled)
			row("ollama_pool", view.OllamaPoolEnabled)
			row("cluster", view.Cluster.Enabled)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	return cmd
}

func builtinsSetCmd() *cobra.Command {
	var (
		shell, desktop, clipboard, ssh, sshLocalFwd, sshRemoteFwd, sshAgentFwd, sftp, podman, ollama, ollamaPool, cluster string
		sshForwardSockets                                                                                                 []string
		clearSSHForwardSockets                                                                                            bool
	)
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Toggle one or more built-ins. Only flags actually passed are modified.",
		Long: `Each flag accepts on|off|true|false|1|0. Example:

  outpost builtins set --ssh=off --sftp=off
  outpost builtins set --ollama=on --ollama-pool=on
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			params := admincore.BuiltinsParams{}
			var err error
			if params.Shell, err = parseToggle("shell", shell); err != nil {
				return err
			}
			if params.Desktop, err = parseToggle("desktop", desktop); err != nil {
				return err
			}
			if params.Clipboard, err = parseToggle("clipboard", clipboard); err != nil {
				return err
			}
			if params.SSH, err = parseToggle("ssh", ssh); err != nil {
				return err
			}
			if params.SSHAllowLocalForward, err = parseToggle("ssh-local-fwd", sshLocalFwd); err != nil {
				return err
			}
			if params.SSHAllowRemoteForward, err = parseToggle("ssh-remote-fwd", sshRemoteFwd); err != nil {
				return err
			}
			if params.SSHAllowAgentForward, err = parseToggle("ssh-agent-fwd", sshAgentFwd); err != nil {
				return err
			}
			if params.SFTP, err = parseToggle("sftp", sftp); err != nil {
				return err
			}
			if params.Podman, err = parseToggle("podman", podman); err != nil {
				return err
			}
			if params.Ollama, err = parseToggle("ollama", ollama); err != nil {
				return err
			}
			if params.OllamaPool, err = parseToggle("ollama-pool", ollamaPool); err != nil {
				return err
			}
			if params.Cluster, err = parseToggle("cluster", cluster); err != nil {
				return err
			}
			if clearSSHForwardSockets {
				params.SSHForwardSockets = []string{}
			} else if cmd.Flags().Changed("ssh-forward-socket") {
				params.SSHForwardSockets = sshForwardSockets
			}
			return runSetBuiltins(cmd.Context(), params)
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "on|off")
	cmd.Flags().StringVar(&desktop, "desktop", "", "on|off")
	cmd.Flags().StringVar(&clipboard, "clipboard", "", "on|off")
	cmd.Flags().StringVar(&ssh, "ssh", "", "on|off")
	cmd.Flags().StringVar(&sshLocalFwd, "ssh-local-fwd", "", "on|off — direct-tcpip channels (ssh -L)")
	cmd.Flags().StringVar(&sshRemoteFwd, "ssh-remote-fwd", "", "on|off — tcpip-forward global requests (ssh -R)")
	cmd.Flags().StringVar(&sshAgentFwd, "ssh-agent-fwd", "", "on|off — auth-agent-req channels (ssh -A)")
	cmd.Flags().StringVar(&sftp, "sftp", "", "on|off")
	cmd.Flags().StringVar(&podman, "podman", "", "on|off")
	cmd.Flags().StringVar(&ollama, "ollama", "", "on|off")
	cmd.Flags().StringVar(&ollamaPool, "ollama-pool", "", "on|off — share local Ollama with cloudbox's pool")
	cmd.Flags().StringVar(&cluster, "cluster", "", "on|off — join cloudbox virtual-podman cluster")
	cmd.Flags().StringSliceVar(&sshForwardSockets, "ssh-forward-socket", nil, "Allow this unix-socket path for SSH direct-streamlocal forwarding (repeatable; replaces the entire list)")
	cmd.Flags().BoolVar(&clearSSHForwardSockets, "clear-ssh-forward-sockets", false, "Reset ssh-forward-sockets to the auto-detect default set")
	return cmd
}

func runSetBuiltins(ctx context.Context, params admincore.BuiltinsParams) error {
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	var out struct {
		RestartPending bool `json:"restart_pending"`
	}
	if err := session.callTool(ctx, "outpost_set_builtins", params, &out); err != nil {
		return err
	}
	if out.RestartPending {
		fmt.Println("Saved. Restarting outpost — poll `outpost status` until configured=true.")
	} else {
		fmt.Println("Saved.")
	}
	return nil
}

// parseToggle converts a CLI flag value into a *bool. Empty string means
// "flag not set" (nil pointer); anything else must parse to a known
// truthy/falsey form.
func parseToggle(name, raw string) (*bool, error) {
	switch raw {
	case "":
		return nil, nil
	case "on", "true", "1", "yes":
		t := true
		return &t, nil
	case "off", "false", "0", "no":
		f := false
		return &f, nil
	}
	return nil, fmt.Errorf("--%s: invalid value %q (expected on|off)", name, raw)
}
