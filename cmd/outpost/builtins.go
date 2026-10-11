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
			row("files", view.FilesEnabled)
			row("files_allow_write", view.FilesAllowWrite)
			scope := view.FilesScope
			if scope == "" {
				scope = "(home)"
			}
			fmt.Printf("  %-22s  %s\n", "files_scope", scope)
			row("podman", view.Podman.Enabled)
			row("sandbox", view.Sandbox.Enabled)
			row("ollama", view.Ollama.Enabled)
			row("ollama_pool", view.OllamaPoolEnabled)
			row("warm_serving", view.WarmServingEnabled)
			fmt.Printf("  %-22s  %.2f\n", "warm_budget_frac", view.WarmBudgetFrac)
			row("lan_inference", view.LANInferenceEnabled)
			row("mesh", view.MeshEnabled)
			row("zot", view.ZotEnabled)
			row("seaweedfs", view.SeaweedfsEnabled)
			row("kopia", view.KopiaEnabled)
			row("actrunner", view.ActrunnerEnabled)
			row("otel", view.OtelEnabled)
			row("otel_pool", view.OtelPoolEnabled)
			row("ycode_share", view.YcodeShareEnabled)
			row("ycode_share_require_login", view.YcodeShareRequireLogin)
			for _, s := range view.YcodeShareSurfaces {
				row("  "+s.Name, s.Enabled)
			}
			row("cluster", view.Cluster.Enabled)
			row("cluster_agent", view.Cluster.Agent)
			fmt.Printf("  %-22s  %s\n", "cluster_virtual", strings.Join(view.Cluster.Virtual, ","))
			// Services are DECLARATIONS, not built-in toggles: one row per
			// unit the host knows about, whatever its provider or origin.
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
		shell, desktop, clipboard, ssh, sshLocalFwd, sshRemoteFwd, sshAgentFwd, sftp, podman, sandbox, ollama, ollamaPool, otel, otelPool, ycodeShare, ycodeShareRequireLogin, cluster string
		files, filesAllowWrite, filesScope                                                                                                                                             string
		clusterAgent                                                                                                                                                                   string
		clusterVirtual                                                                                                                                                                 []string
		updateMode, autoUpgradeLegacy, autoRollback                                                                                                                                    string
		warmServing                                                                                                                                                                    string
		warmBudgetFrac                                                                                                                                                                 float64
		mesh                                                                                                                                                                           string
		meshPort                                                                                                                                                                       int
		lanInference                                                                                                                                                                   string
		lanInferencePort                                                                                                                                                               int
		services                                                                                                                                                                       []string
		serviceSets                                                                                                                                                                    []string
		bashyVersion                                                                                                                                                                   string
		zot                                                                                                                                                                            string
		zotPort                                                                                                                                                                        int
		seaweedfs                                                                                                                                                                      string
		seaweedfsPort                                                                                                                                                                  int
		kopia                                                                                                                                                                          string
		kopiaPort                                                                                                                                                                      int
		actrunner                                                                                                                                                                      string
		actrunnerInstance                                                                                                                                                              string
		actrunnerToken                                                                                                                                                                 string
		cloudDO                                                                                                                                                                        string
		cloudDOToken                                                                                                                                                                   string
		actrunnerLabels                                                                                                                                                                string
		actrunnerSandbox                                                                                                                                                               string
		actrunnerSandboxImage                                                                                                                                                          string
		actrunnerDockerHost                                                                                                                                                            string
		headlamp                                                                                                                                                                       string
		headlampPort                                                                                                                                                                   int
		sshForwardSockets                                                                                                                                                              []string
		clearSSHForwardSockets                                                                                                                                                         bool
		ycodeShareSurfaces                                                                                                                                                             map[string]string
	)
	var (
		shard      string
		shardRole  string
		shardPeers []string
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
			if params.Files, err = parseToggle("files", files); err != nil {
				return err
			}
			if params.FilesAllowWrite, err = parseToggle("files-allow-write", filesAllowWrite); err != nil {
				return err
			}
			if cmd.Flags().Changed("files-scope") {
				params.FilesScope = &filesScope
			}
			if params.Podman, err = parseToggle("podman", podman); err != nil {
				return err
			}
			if params.Sandbox, err = parseToggle("sandbox", sandbox); err != nil {
				return err
			}
			if params.Ollama, err = parseToggle("ollama", ollama); err != nil {
				return err
			}
			if params.OllamaPool, err = parseToggle("ollama-pool", ollamaPool); err != nil {
				return err
			}
			if params.WarmServing, err = parseToggle("warm-serving", warmServing); err != nil {
				return err
			}
			if cmd.Flags().Changed("warm-budget-frac") {
				params.WarmBudgetFrac = &warmBudgetFrac
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
			if params.LANInference, err = parseToggle("lan-inference", lanInference); err != nil {
				return err
			}
			if cmd.Flags().Changed("lan-inference-port") {
				params.LANInferencePort = &lanInferencePort
			}
			if params.Shard, err = parseToggle("shard", shard); err != nil {
				return err
			}
			if cmd.Flags().Changed("shard-peers") {
				params.ShardPeers = shardPeers
			}
			if cmd.Flags().Changed("shard-role") {
				params.ShardRole = &shardRole
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
			if params.Zot, err = parseToggle("zot", zot); err != nil {
				return err
			}
			if cmd.Flags().Changed("zot-port") {
				params.ZotPort = &zotPort
			}
			if params.Seaweedfs, err = parseToggle("seaweedfs", seaweedfs); err != nil {
				return err
			}
			if cmd.Flags().Changed("seaweedfs-port") {
				params.SeaweedfsPort = &seaweedfsPort
			}
			if params.Kopia, err = parseToggle("kopia", kopia); err != nil {
				return err
			}
			if cmd.Flags().Changed("kopia-port") {
				params.KopiaPort = &kopiaPort
			}
			if params.Actrunner, err = parseToggle("actrunner", actrunner); err != nil {
				return err
			}
			if cmd.Flags().Changed("actrunner-instance") {
				params.ActrunnerInstance = &actrunnerInstance
			}
			if cmd.Flags().Changed("actrunner-token") {
				params.ActrunnerToken = &actrunnerToken
			}
			if params.CloudDOEnabled, err = parseToggle("cloud-do", cloudDO); err != nil {
				return err
			}
			if cmd.Flags().Changed("cloud-do-token") {
				params.CloudDOToken = &cloudDOToken
			}
			if cmd.Flags().Changed("actrunner-labels") {
				params.ActrunnerLabels = &actrunnerLabels
			}
			if params.ActrunnerSandbox, err = parseToggle("actrunner-sandbox", actrunnerSandbox); err != nil {
				return err
			}
			if cmd.Flags().Changed("actrunner-sandbox-image") {
				params.ActrunnerSandboxImage = &actrunnerSandboxImage
			}
			if cmd.Flags().Changed("actrunner-docker-host") {
				params.ActrunnerDockerHost = &actrunnerDockerHost
			}
			if params.Headlamp, err = parseToggle("headlamp", headlamp); err != nil {
				return err
			}
			if cmd.Flags().Changed("headlamp-port") {
				params.HeadlampPort = &headlampPort
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
	cmd.Flags().StringVar(&shard, "shard", "", "on|off — Ollama sharding; default on for a paired node, this opts out")
	cmd.Flags().StringSliceVar(&shardPeers, "shard-peers", nil, "worker hostnames (empty/auto = every same-LAN peer)")
	cmd.Flags().StringVar(&shardRole, "shard-role", "", "auto|leader|worker")
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
	cmd.Flags().StringVar(&files, "files", "", "on|off — built-in File Browser (remote view/download)")
	cmd.Flags().StringVar(&filesAllowWrite, "files-allow-write", "", "on|off — allow upload/edit/rename/delete in File Browser (default off = read-only; LAN-gated control)")
	cmd.Flags().StringVar(&filesScope, "files-scope", "", "root directory File Browser is confined to (empty = OS user home)")
	cmd.Flags().StringVar(&podman, "podman", "", "on|off — raw admin-only podman passthrough (default on; skipped when no podman socket is found)")
	cmd.Flags().StringVar(&sandbox, "sandbox", "", "on|off — filtered container sandbox (strips privileged/host-ns/binds/caps/devices; needs podman)")
	cmd.Flags().StringVar(&ollama, "ollama", "", "on|off — built-in Ollama proxy (default on; skipped when no Ollama daemon is found)")
	cmd.Flags().StringVar(&ollamaPool, "ollama-pool", "", "on|off — share local Ollama with cloudbox's pool")
	cmd.Flags().StringVar(&warmServing, "warm-serving", "", "on|off — considerate always-on warm serving: keep a small model set resident, yield when the host is busy (default on for a paired Ollama node)")
	cmd.Flags().Float64Var(&warmBudgetFrac, "warm-budget-frac", 0, "fraction of usable memory dedicated to warm preload (0<frac<=1; default 0.33; drops to 0 when the host is busy)")
	cmd.Flags().StringVar(&otel, "otel", "", "on|off — expose the local embedded Prom/Alertmanager/VLogs/Jaeger stack as built-in apps (default on; skipped when no local observability proxy is found)")
	cmd.Flags().StringVar(&otelPool, "otel-pool", "", "on|off — allow cloudbox to federate queries across this host's observability stack")
	cmd.Flags().StringVar(&ycodeShare, "ycode-share", "", "on|off — expose ycode's home/landing page through the matrix tunnel (default on when ycode is on)")
	cmd.Flags().StringVar(&ycodeShareRequireLogin, "ycode-share-require-login", "", "on|off — require cloudbox OS-password elevation for the 'ycode' app (default off; on = OS password popup like /shell)")
	cmd.Flags().StringToStringVar(&ycodeShareSurfaces, "ycode-share-surface", nil, "Toggle a ycode-share surface, repeatable: --ycode-share-surface ycode-canvas=on --ycode-share-surface ycode-git=on")
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
	cmd.Flags().StringVar(&lanInference, "lan-inference", "", "on|off - serve local LLM inference directly to same-LAN callers (LAN-TRUST: no per-request auth; needs Ollama on + pairing; default off)")
	cmd.Flags().IntVar(&lanInferencePort, "lan-inference-port", 0, "TCP port the LAN inference listener binds on 0.0.0.0 (0 = default 11436; must differ from the inference server's 11434 and bashy's own 11435 engine)")
	cmd.Flags().StringSliceVar(&services, "service", nil, "NAME=on|off — desired state of a service unit (repeatable). The same write `bashy service enable|disable` performs; applies live, no outpost restart. `bashy service list` names the units.")
	cmd.Flags().StringSliceVar(&serviceSets, "service-set", nil, "NAME.PATH=VALUE — override a field of a unit's declaration (repeatable), e.g. loom.ports.0.port=31900. The `bashy service set` write.")
	cmd.Flags().StringVar(&bashyVersion, "bashy-version", "", "pin the bashy release the daemon auto-installs when bashy is missing (empty/'latest' = newest; e.g. v0.3.0). Pin in production.")
	cmd.Flags().StringVar(&zot, "zot", "", "on|off - run the Zot OCI registry (managed external binary) on loopback, auto-exposed over the mesh as 'registry'")
	cmd.Flags().IntVar(&zotPort, "zot-port", 0, "zot's loopback HTTP port (0 = default 5000)")
	cmd.Flags().StringVar(&seaweedfs, "seaweedfs", "", "on|off - run SeaweedFS (object/blob store, S3 gateway; managed external binary) on loopback, auto-exposed over the mesh as 's3'")
	cmd.Flags().IntVar(&seaweedfsPort, "seaweedfs-port", 0, "SeaweedFS's loopback S3-gateway port (0 = default 8333)")
	cmd.Flags().StringVar(&kopia, "kopia", "", "on|off - run the Kopia snapshot-backup repo server (managed external binary) on loopback, auto-exposed over the mesh as 'backup'")
	cmd.Flags().IntVar(&kopiaPort, "kopia-port", 0, "Kopia's loopback server port (0 = default 51515)")
	cmd.Flags().StringVar(&headlamp, "headlamp", "", "on|off - deploy Headlamp (the operating UI) on the peer DKS plane (default OFF; requires cluster.control_plane)")
	cmd.Flags().IntVar(&headlampPort, "headlamp-port", 0, "headlamp's supervised loopback forward port (0 = default 18466; loopback only)")
	cmd.Flags().StringVar(&actrunner, "actrunner", "", "on|off - run Gitea act_runner (CI executor, managed external binary); registers against a Gitea instance and dials out")
	cmd.Flags().StringVar(&actrunnerInstance, "actrunner-instance", "", "Gitea base URL the runner registers against (empty = local loom forge)")
	cmd.Flags().StringVar(&actrunnerToken, "actrunner-token", "", "act_runner registration token (minted in Gitea)")
	cmd.Flags().StringVar(&cloudDO, "cloud-do", "", "on|off - Digital Ocean provider support (exports the token as DIGITALOCEAN_ACCESS_TOKEN for bashy doctl)")
	cmd.Flags().StringVar(&cloudDOToken, "cloud-do-token", "", "Digital Ocean API token (exported as DIGITALOCEAN_ACCESS_TOKEN)")
	cmd.Flags().StringVar(&actrunnerLabels, "actrunner-labels", "", "executor labels (default 'host:host')")
	cmd.Flags().StringVar(&actrunnerSandbox, "actrunner-sandbox", "", "on|off - also offer the tier-3 sandbox (container) executor: runs-on:sandbox → OCI container via bashy podman (additive to the host lane)")
	cmd.Flags().StringVar(&actrunnerSandboxImage, "actrunner-sandbox-image", "", "OCI image for the sandbox executor (empty = a node image with git+node+bash)")
	cmd.Flags().StringVar(&actrunnerDockerHost, "actrunner-docker-host", "", "DOCKER_HOST for the sandbox executor (empty = auto-resolve bashy podman's socket)")
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
