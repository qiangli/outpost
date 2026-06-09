package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// outpost builtins {show,set} — CLI mirror of the SPA's built-in app
// toggles. `set` accepts each switch as an --<name>=on/off flag; only
// flags actually passed are sent to the daemon (pointer-bool semantics).
func builtinsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "builtins",
		Short: "Show or toggle built-in routes (shell/desktop/clipboard/ssh/sftp/podman/ollama/ollama-pool/otel/ycode-share/cluster)",
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
			row("otel", view.OtelEnabled)
			row("otel_pool", view.OtelPoolEnabled)
			row("ycode_share", view.YcodeShareEnabled)
			row("ycode_share_require_login", view.YcodeShareRequireLogin)
			for _, s := range view.YcodeShareSurfaces {
				row("  "+s.Name, s.Enabled)
			}
			row("cluster", view.Cluster.Enabled)
			mode := view.Cluster.Mode
			if mode == "" {
				mode = "agent"
			}
			fmt.Printf("  %-22s  %s\n", "cluster_mode", mode)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	return cmd
}

func builtinsSetCmd() *cobra.Command {
	var (
		shell, desktop, clipboard, ssh, sshLocalFwd, sshRemoteFwd, sshAgentFwd, sftp, podman, ollama, ollamaPool, otel, otelPool, ycodeShare, ycodeShareRequireLogin, cluster string
		clusterMode                                                                                                                                                           string
		updateMode, autoUpgradeLegacy                                                                                                                                         string
		sshForwardSockets                                                                                                                                                     []string
		clearSSHForwardSockets                                                                                                                                                bool
		ycodeShareSurfaces                                                                                                                                                    map[string]string
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
			if params.SSHAllowLocalForward, err = parseToggle("ssh-allow-local-forward", sshLocalFwd); err != nil {
				return err
			}
			if params.SSHAllowRemoteForward, err = parseToggle("ssh-allow-remote-forward", sshRemoteFwd); err != nil {
				return err
			}
			if params.SSHAllowAgentForward, err = parseToggle("ssh-allow-agent-forward", sshAgentFwd); err != nil {
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
			if params.Otel, err = parseToggle("otel", otel); err != nil {
				return err
			}
			if params.OtelPool, err = parseToggle("otel-pool", otelPool); err != nil {
				return err
			}
			if params.YcodeShare, err = parseToggle("ycode-share", ycodeShare); err != nil {
				return err
			}
			if params.YcodeShareRequireLogin, err = parseToggle("ycode-share-require-login", ycodeShareRequireLogin); err != nil {
				return err
			}
			if len(ycodeShareSurfaces) > 0 {
				params.YcodeShareSurfaces = map[string]bool{}
				for k, v := range ycodeShareSurfaces {
					b, perr := parseToggle("ycode-share-surface "+k, v)
					if perr != nil {
						return perr
					}
					if b != nil {
						params.YcodeShareSurfaces[k] = *b
					}
				}
			}
			if params.Cluster, err = parseToggle("cluster", cluster); err != nil {
				return err
			}
			if clusterMode != "" {
				m := strings.ToLower(strings.TrimSpace(clusterMode))
				if m != "vkpodman" && m != "agent" {
					return fmt.Errorf("--cluster-mode must be vkpodman|agent, got %q", clusterMode)
				}
				params.ClusterMode = &m
			}
			// --update=auto|manual|never is the canonical knob; the
			// deprecated --auto-upgrade=on|off folds in as auto/never.
			if updateMode != "" {
				m := strings.ToLower(strings.TrimSpace(updateMode))
				if m != "auto" && m != "manual" && m != "never" {
					return fmt.Errorf("--update must be one of auto / manual / never, got %q", updateMode)
				}
				params.UpdateMode = &m
			} else if autoUpgradeLegacy != "" {
				b, err := parseToggle("auto-upgrade", autoUpgradeLegacy)
				if err != nil {
					return err
				}
				if b != nil {
					m := "never"
					if *b {
						m = "auto"
					}
					params.UpdateMode = &m
				}
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
	// Canonical names match the file/MCP key (ssh_allow_local_forward etc.);
	// the old shorter spellings stay as deprecated aliases so existing
	// scripts don't break.
	cmd.Flags().StringVar(&sshLocalFwd, "ssh-allow-local-forward", "", "on|off — direct-tcpip channels for `ssh -L` and direct-streamlocal@openssh.com for podman/docker sockets")
	cmd.Flags().StringVar(&sshLocalFwd, "ssh-local-fwd", "", "deprecated alias for --ssh-allow-local-forward")
	_ = cmd.Flags().MarkDeprecated("ssh-local-fwd", "use --ssh-allow-local-forward")
	cmd.Flags().StringVar(&sshRemoteFwd, "ssh-allow-remote-forward", "", "on|off — tcpip-forward global requests (ssh -R)")
	cmd.Flags().StringVar(&sshRemoteFwd, "ssh-remote-fwd", "", "deprecated alias for --ssh-allow-remote-forward")
	_ = cmd.Flags().MarkDeprecated("ssh-remote-fwd", "use --ssh-allow-remote-forward")
	cmd.Flags().StringVar(&sshAgentFwd, "ssh-allow-agent-forward", "", "on|off — auth-agent-req channels (ssh -A)")
	cmd.Flags().StringVar(&sshAgentFwd, "ssh-agent-fwd", "", "deprecated alias for --ssh-allow-agent-forward")
	_ = cmd.Flags().MarkDeprecated("ssh-agent-fwd", "use --ssh-allow-agent-forward")
	cmd.Flags().StringVar(&sftp, "sftp", "", "on|off")
	cmd.Flags().StringVar(&podman, "podman", "", "on|off")
	cmd.Flags().StringVar(&ollama, "ollama", "", "on|off")
	cmd.Flags().StringVar(&ollamaPool, "ollama-pool", "", "on|off — share local Ollama with cloudbox's pool")
	cmd.Flags().StringVar(&otel, "otel", "", "on|off — expose ycode's embedded Prom/Alertmanager/VLogs/Jaeger as built-in apps")
	cmd.Flags().StringVar(&otelPool, "otel-pool", "", "on|off — allow cloudbox to federate queries across this host's observability stack")
	cmd.Flags().StringVar(&ycodeShare, "ycode-share", "", "on|off — expose ycode's home/landing page through the matrix tunnel (default on when ycode is on)")
	cmd.Flags().StringVar(&ycodeShareRequireLogin, "ycode-share-require-login", "", "on|off — require cloudbox OS-password elevation for the 'ycode' app (default off; on = OS password popup like /shell)")
	cmd.Flags().StringToStringVar(&ycodeShareSurfaces, "ycode-share-surface", nil, "Toggle a ycode-share surface, repeatable: --ycode-share-surface ycode-canvas=on --ycode-share-surface ycode-git=on")
	cmd.Flags().StringVar(&cluster, "cluster", "", "on|off — join cloudbox virtual-podman cluster")
	cmd.Flags().StringVar(&clusterMode, "cluster-mode", "", "vkpodman|agent — agent (default; real k3s-agent in the outpost-runtime container, conformance-track) or vkpodman (v1 virtual-kubelet shim, kept for outposts that integrate with host-side podman tooling outside K8s)")
	cmd.Flags().StringVar(&updateMode, "update", "", "auto|manual|never — policy for cloudbox-pushed self-upgrades")
	cmd.Flags().StringVar(&autoUpgradeLegacy, "auto-upgrade", "", "deprecated alias for --update (on→auto, off→never)")
	_ = cmd.Flags().MarkDeprecated("auto-upgrade", "use --update=auto|manual|never")
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
