package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// outpost builtins {show,set} — CLI mirror of the SPA's built-in app
// toggles. `set` accepts each switch as an --<name>=on/off flag; only
// flags actually passed are sent to the daemon (pointer-bool semantics).
func builtinsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "builtins",
		Short: "Show or toggle built-in routes (shell/desktop/clipboard/ssh/sftp), the mesh, the cluster join, and the desired state of service units",
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
			row("mesh", view.MeshEnabled)
			row("cluster", view.Cluster.Enabled)
			row("cluster_agent", view.Cluster.Agent)
			fmt.Printf("  %-22s  %s\n", "cluster_virtual", strings.Join(view.Cluster.Virtual, ","))
			// Local-daemon DETECTION, not a toggle: whether outpost proxies
			// podman or Ollama is its unit's desired state, printed below.
			fmt.Println("Detected on this host:")
			detected := func(name string, v admincore.BuiltinView) {
				target := v.Target
				if target == "" {
					target = "-"
				}
				fmt.Printf("  %-22s  %-5t  %s\n", name, v.Available, target)
			}
			detected("podman", view.Podman)
			detected("ollama", view.Ollama)
			// Services are DECLARATIONS, not built-in toggles: one row per
			// unit the host knows about, whatever its provider or origin —
			// including the ones outpost itself hosts in-process (the File
			// Browser, the container mounts, the Ollama proxy and pool, the
			// warm and shard planes, the LAN inference listener, the
			// observability and ycode surfaces).
			// `outpost services` shows the monitor's live state as well.
			if len(view.Services) > 0 {
				fmt.Println("Service units (desired state; `outpost services` for live state):")
				for _, u := range view.Services {
					expose := u.App
					if len(u.Mesh) > 0 {
						expose = strings.TrimPrefix(expose+" mesh:"+strings.Join(u.Mesh, "+"), " ")
					}
					if expose == "" {
						expose = "-"
					}
					fmt.Printf("  %-22s  %-5t  %-9s  %s\n", u.Name, u.Enabled, u.Provider, expose)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	return cmd
}

func builtinsSetCmd() *cobra.Command {
	var (
		shell, desktop, clipboard, ssh, sshLocalFwd, sshRemoteFwd, sshAgentFwd, sftp, cluster string
		clusterAgent                                                                          string
		clusterVirtual                                                                        []string
		updateMode, autoUpgradeLegacy, autoRollback                                           string
		mesh                                                                                  string
		meshPort                                                                              int
		services                                                                              []string
		serviceSets                                                                           []string
		bashyVersion                                                                          string
		cloudDO                                                                               string
		cloudDOToken                                                                          string
		sshForwardSockets                                                                     []string
		clearSSHForwardSockets                                                                bool
	)
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Toggle one or more built-ins. Only flags actually passed are modified.",
		Long: `Each flag accepts on|off|true|false|1|0. Example:

  outpost builtins set --ssh=off --sftp=off

Everything outpost used to carry its own flag for is a service unit now —
switch it with --service and configure it with --service-set:

  outpost builtins set --service files=on --service-set files.settings.allow_write=true
  outpost builtins set --service ollama=on --service ollama-pool=on
  outpost builtins set --service warm=on --service-set warm.settings.budget_frac=0.5
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
			if params.Cluster, err = parseToggle("cluster", cluster); err != nil {
				return err
			}
			if params.ClusterAgent, err = parseToggle("cluster-agent", clusterAgent); err != nil {
				return err
			}
			if cmd.Flags().Changed("cluster-virtual") {
				for _, mode := range clusterVirtual {
					if !conf.ValidVirtualRuntime(strings.ToLower(strings.TrimSpace(mode))) {
						return fmt.Errorf("--cluster-virtual entries must be vk-podman, vk-native, or vk-ollama; got %q", mode)
					}
				}
				params.ClusterVirtual = clusterVirtual
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
			if params.AutoRollback, err = parseToggle("auto-rollback", autoRollback); err != nil {
				return err
			}
			if params.Mesh, err = parseToggle("mesh", mesh); err != nil {
				return err
			}
			if cmd.Flags().Changed("mesh-port") {
				params.MeshPort = &meshPort
			}
			if len(services) > 0 {
				if params.Services, err = parseServiceToggles(services); err != nil {
					return err
				}
			}
			if len(serviceSets) > 0 {
				if params.ServiceSettings, err = parseServiceSets(serviceSets); err != nil {
					return err
				}
			}
			if cmd.Flags().Changed("bashy-version") {
				params.BashyVersion = &bashyVersion
			}
			if params.CloudDOEnabled, err = parseToggle("cloud-do", cloudDO); err != nil {
				return err
			}
			if cmd.Flags().Changed("cloud-do-token") {
				params.CloudDOToken = &cloudDOToken
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
	cmd.Flags().StringVar(&cluster, "cluster", "", "on|off — join cloudbox virtual-podman cluster (opt-in: hands a remote control plane the right to schedule work on this host)")
	cmd.Flags().StringVar(&clusterAgent, "cluster-agent", "", "on|off — run one real k3s-agent Node")
	cmd.Flags().StringSliceVar(&clusterVirtual, "cluster-virtual", nil, "virtual-kubelet backends to run concurrently: vk-podman,vk-native,vk-ollama (replaces the complete set)")
	cmd.Flags().StringVar(&updateMode, "update", "", "auto|manual|never — policy for cloudbox-pushed self-upgrades")
	cmd.Flags().StringVar(&autoUpgradeLegacy, "auto-upgrade", "", "deprecated alias for --update (on→auto, off→never)")
	_ = cmd.Flags().MarkDeprecated("auto-upgrade", "use --update=auto|manual|never")
	cmd.Flags().StringVar(&autoRollback, "auto-rollback", "", "on|off — arm the auto-rollback watchdog's destructive revert (default off / observe-only)")
	cmd.Flags().StringSliceVar(&sshForwardSockets, "ssh-forward-socket", nil, "Allow this unix-socket path for SSH direct-streamlocal forwarding (repeatable; replaces the entire list)")
	cmd.Flags().BoolVar(&clearSSHForwardSockets, "clear-ssh-forward-sockets", false, "Reset ssh-forward-sockets to the auto-detect default set")
	cmd.Flags().StringVar(&mesh, "mesh", "", "on|off - libp2p mesh data plane (peer-to-peer transport under shard-RPC/peer-backup; needs pairing)")
	cmd.Flags().IntVar(&meshPort, "mesh-port", 0, "TCP+QUIC listen port for the mesh host (0 = ephemeral)")
	cmd.Flags().StringSliceVar(&services, "service", nil, "NAME=on|off — desired state of a service unit (repeatable). The same write `bashy service enable|disable` performs; applies live, no outpost restart. `bashy service list` names the units.")
	cmd.Flags().StringSliceVar(&serviceSets, "service-set", nil, "NAME.PATH=VALUE — override a field of a unit's declaration (repeatable), e.g. loom.ports.0.port=31900. The `bashy service set` write.")
	cmd.Flags().StringVar(&bashyVersion, "bashy-version", "", "pin the bashy release the daemon auto-installs when bashy is missing (empty/'latest' = newest; e.g. v0.3.0). Pin in production.")
	cmd.Flags().StringVar(&cloudDO, "cloud-do", "", "on|off - Digital Ocean provider support (exports the token as DIGITALOCEAN_ACCESS_TOKEN for bashy doctl)")
	cmd.Flags().StringVar(&cloudDOToken, "cloud-do-token", "", "Digital Ocean API token (exported as DIGITALOCEAN_ACCESS_TOKEN)")
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

// parseServiceToggles reads the repeatable `--service NAME=on|off` flag into
// the generic desired-state map. One flag covers every service — a seeded
// built-in, an in-process outpost capability, a third-party unit an operator
// declared this morning — which is why `--loom`, `--meet` and `--bashy-apps`
// are gone rather than joined by a fourth.
func parseServiceToggles(entries []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, e := range entries {
		name, value, ok := strings.Cut(e, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" {
			return nil, fmt.Errorf("--service %q: expected NAME=on|off", e)
		}
		on, err := parseToggle("service "+name, value)
		if err != nil {
			return nil, err
		}
		if on == nil {
			return nil, fmt.Errorf("--service %q: expected on|off", e)
		}
		out[name] = *on
	}
	return out, nil
}

// parseServiceSets reads the repeatable `--service-set NAME.PATH=VALUE` flag.
// The unit name is the first dotted segment; the rest is the path into the
// declaration, exactly as `bashy service set NAME --set PATH=VALUE` takes it.
func parseServiceSets(entries []string) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for _, e := range entries {
		lhs, value, ok := strings.Cut(e, "=")
		if !ok {
			return nil, fmt.Errorf("--service-set %q: expected NAME.PATH=VALUE", e)
		}
		name, path, ok := strings.Cut(strings.TrimSpace(lhs), ".")
		if !ok || name == "" || path == "" {
			return nil, fmt.Errorf("--service-set %q: expected NAME.PATH=VALUE, e.g. loom.ports.0.port=31900", e)
		}
		if out[name] == nil {
			out[name] = map[string]string{}
		}
		out[name][path] = value
	}
	return out, nil
}
