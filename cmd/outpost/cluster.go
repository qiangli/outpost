package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/runtime"
	"github.com/qiangli/outpost/internal/agent/userkube"
	"github.com/qiangli/outpost/internal/agent/vknode"
)

// clusterLeaveCmd is the clean "leave DKS" lifecycle command. It tells cloudbox
// to reclaim this host's cluster state — its k8s Node(s), overlay registration,
// and pod-CIDR reservation — then disables cluster mode and stops the local
// runtime. A subsequent `outpost cluster join` rejoins from a clean slate
// (fresh pod CIDR, new node, new overlay identity) rather than reusing stale
// state. This is the missing half of node lifecycle: today a node can only
// DRAIN (depart), never actually leave and reclaim.
func clusterLeaveCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "leave",
		Short: "Leave the DKS cluster: reclaim this host's node + overlay + pod CIDR on cloudbox, then stop the local runtime",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				fmt.Println("This removes this host's k8s node, overlay registration, and pod CIDR on cloudbox,")
				fmt.Println("and disables cluster mode locally. Rejoin with `outpost cluster join`.")
				fmt.Println("Re-run with --yes to confirm.")
				return nil
			}
			cfgPath, err := conf.DefaultConfigPath()
			if err != nil {
				return err
			}
			fc, err := conf.LoadFile(cfgPath)
			if err != nil {
				return fmt.Errorf("load %s: %w", cfgPath, err)
			}
			if fc == nil || fc.AccessToken == "" || fc.AgentName == "" {
				return errors.New("not paired — no access_token/agent_name saved")
			}
			cloudboxBase := cloudboxHTTPBase(fc)
			if cloudboxBase == "" {
				return errors.New("no cloudbox URL in saved config")
			}
			// 1. cloudbox teardown. Best-effort: report failure but still do the
			//    local teardown so `leave` never strands a half-left node.
			if summary, lerr := notifyLeave(cmd.Context(), cloudboxBase, fc.AccessToken, fc.AgentName); lerr != nil {
				fmt.Printf("cloudbox teardown failed: %v (continuing local teardown)\n", lerr)
			} else {
				fmt.Printf("cloudbox: %s\n", summary)
			}
			// 2. local: disable cluster mode PRESERVING the node's mode + identity
			//    (so a rejoin comes back as the same kind of node), then trigger a
			//    real restart so the cluster-off boot tears the runtime container
			//    down. Without the explicit restart the stale k3s kubelet retry-
			//    loops forever on the Node cloudbox just deleted — the leave tool
			//    only sets RestartPending, it deliberately doesn't self-restart.
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				RestartPending bool `json:"restart_pending"`
			}
			if err := session.callTool(cmd.Context(), "outpost_cluster_leave", map[string]any{}, &out); err != nil {
				return err
			}
			if out.RestartPending {
				if err := restartViaMCP(cmd.Context()); err != nil {
					return fmt.Errorf("cloudbox teardown + local disable done, but the restart that stops the runtime failed (run `outpost restart`): %w", err)
				}
			}
			fmt.Println("local: cluster mode disabled (mode preserved); runtime stopping via restart. Rejoin with `outpost cluster join`.")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	return cmd
}

// clusterJoinCmd covers both levels of "join":
//
//	outpost cluster join                       # rejoin the plane already configured
//	outpost cluster join <endpoint> --token …  # join a PEER-hosted plane
//
// The bare form re-enables cluster mode; the daemon re-fetches its kubeconfig
// on the next boot and re-registers, so cloudbox allocates a FRESH pod CIDR
// (and persists it), a new k8s Node, and a new overlay node. The symmetric
// partner to `outpost cluster leave`.
//
// The endpoint form points this host at a control plane hosted by a peer
// outpost instead of by cloudbox — see cluster_join.go for the credential
// handling. One verb rather than two because the operator intent is the same;
// the argument only says which plane.
func clusterJoinCmd() *cobra.Command {
	var f clusterJoinFlags
	cmd := &cobra.Command{
		Use:   "join [endpoint]",
		Short: "Join the DKS cluster — the configured plane, or a peer-hosted one given its endpoint",
		Long: `Join a DKS control plane.

With no arguments this rejoins the plane already configured: cluster mode is
re-enabled and the next boot re-registers this node (fresh pod CIDR, k8s node,
overlay registration).

Given an endpoint it joins a control plane hosted by a PEER outpost rather
than by cloudbox. A peer join needs three credentials from the hosting
machine, all of which it prints:

    outpost cluster control-plane token   # endpoint + join token + stcp secret
    outpost cluster token                 # k3s node token

Supply them here as --token / --stcp-secret / --node-token. To keep secrets
out of argv and shell history, --token-stdin reads the join token from stdin
and each credential also has an environment fallback (` +
			envJoinToken + `, ` + envSTCPSecret + `, ` + envNodeToken + `).

RUNTIME SELECTION. --cluster-agent / --cluster-virtual choose which Nodes this
worker registers, spelling cluster.runtimes.agent and cluster.runtimes.virtual.
A peer plane hosts virtual-kubelet nodes as-is (vk authenticates to k3s with
client certificates), so vk-podman / vk-native workloads run on one. Naming
neither keeps the historical default: the agent runtime, and only when no
runtime is selected already — a join never overwrites a selection you made.

Examples:
  outpost cluster join
  outpost cluster join 10.0.0.5:7000 --token T --stcp-secret S --node-token K10…
  outpost cluster token | ssh worker outpost cluster join 10.0.0.5 --token-stdin
  outpost cluster join 10.0.0.5 --token T --cluster-virtual vk-podman,vk-native
  outpost cluster join --cluster-agent=off --cluster-virtual vk-podman  # vk only
  outpost cluster join --show     # which plane this host joins (redacted)
  outpost cluster join --clear    # back to the cloudbox-hosted plane`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			endpoint := ""
			if len(args) == 1 {
				endpoint = args[0]
			}
			switch {
			case f.show:
				return runPeerPlaneShow(cmd.Context(), f.offline)
			case f.clear:
				return runPeerPlaneClear(cmd.Context(), f.offline)
			case wantsPeerJoin(endpoint, &f):
				p, err := resolveJoinParams(cmd, endpoint, &f)
				if err != nil {
					return err
				}
				return runPeerJoin(cmd.Context(), p, f.offline)
			}

			if f.offline {
				return errors.New("--offline applies to a peer join; pass an endpoint (or use --show/--clear)")
			}
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				RestartPending bool `json:"restart_pending"`
			}
			if err := session.callTool(cmd.Context(), "outpost_cluster_join", map[string]any{}, &out); err != nil {
				return err
			}
			if out.RestartPending {
				if err := restartViaMCP(cmd.Context()); err != nil {
					return fmt.Errorf("cluster mode enabled, but the restart that brings the runtime up failed (run `outpost restart`): %w", err)
				}
			}
			fmt.Println("cluster mode enabled (mode preserved); outpost restarting to rejoin fresh. Poll `outpost status`, then `kubectl get nodes`.")
			return nil
		},
	}
	addClusterJoinFlags(cmd, &f)
	return cmd
}

// addClusterJoinFlags registers the join flag set against a command. Split out
// so a test can parse a real argv into a real clusterJoinFlags without
// restating the flag names the operator actually types.
func addClusterJoinFlags(cmd *cobra.Command, f *clusterJoinFlags) {
	cmd.Flags().StringVar(&f.token, "token", "", "Peer's tunnel token (env "+envJoinToken+")")
	cmd.Flags().BoolVar(&f.tokenStdin, "token-stdin", false, "Read the tunnel token from stdin, keeping it out of argv")
	cmd.Flags().StringVar(&f.stcpSecret, "stcp-secret", "", "Peer's STCP secret, which authorizes reaching its apiserver (env "+envSTCPSecret+")")
	cmd.Flags().StringVar(&f.nodeToken, "node-token", "", "Peer's k3s node token, from `outpost cluster token` there (env "+envNodeToken+")")
	cmd.Flags().IntVar(&f.apiPort, "api-port", 0, "Local port this worker binds the joined apiserver on (default 6443)")
	cmd.Flags().BoolVar(&f.offline, "offline", false, "Mutate the FileConfig directly without contacting the daemon (installer-script mode)")
	cmd.Flags().BoolVar(&f.show, "show", false, "Report which control plane this host joins (credentials redacted)")
	cmd.Flags().BoolVar(&f.clear, "clear", false, "Stop joining a peer-hosted plane and revert to the cloudbox-hosted one")
	cmd.Flags().StringVar(&f.clusterAgent, "cluster-agent", "", "on|off — register one real k3s-agent Node on the joined plane (default: on, only when no runtime is selected yet)")
	cmd.Flags().StringSliceVar(&f.clusterVirtual, "cluster-virtual", nil, "virtual-kubelet backends to register on the joined plane: vk-podman,vk-native,vk-ollama (replaces the complete set)")
}

// notifyLeave POSTs /api/v1/cluster/leave and returns cloudbox's teardown
// summary. Mirrors notifyDeparting's auth: Bearer access_token + the
// X-Outpost-Agent header naming which host is leaving.
func notifyLeave(ctx context.Context, cloudboxBase, accessToken, agentName string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(cloudboxBase), "/")
	if base == "" {
		return "", errors.New("notifyLeave: empty cloudboxBase")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/cluster/leave", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-Outpost-Agent", agentName)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("cloudbox responded %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var r struct {
		Message      string   `json:"message"`
		NodesDeleted []string `json:"nodes_deleted"`
		OverlayNodes int      `json:"overlay_nodes_deleted"`
		PodCIDR      bool     `json:"pod_cidr_released"`
	}
	_ = json.Unmarshal(body, &r)
	msg := r.Message
	if msg == "" {
		msg = "left"
	}
	return fmt.Sprintf("%s (k8s nodes=%d, overlay nodes=%d, pod_cidr_released=%v)",
		msg, len(r.NodesDeleted), r.OverlayNodes, r.PodCIDR), nil
}

// clusterCmd is the `outpost cluster <subcommand>` group. Currently
// holds one subcommand (kubeconfig) but structured as a group so
// future additions (e.g. `outpost cluster nodes`, `outpost cluster
// status`) slot in without redefining the surface.
func clusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Interact with this outpost's cloudbox k8s cluster (default mode: real k3s-agent in a podman container; legacy vkpodman is opt-in)",
	}
	cmd.AddCommand(clusterKubeconfigCmd(), clusterUserKubeconfigCmd(),
		clusterInitCmd(), clusterShardInitCmd(), clusterClearCmd(), clusterBuildRuntimeCmd(),
		clusterLeaveCmd(), clusterJoinCmd(), clusterRecipeCmd(),
		clusterControlPlaneCmd(), clusterNodeTokenCmd())
	return cmd
}

// clusterBuildRuntimeCmd builds the outpost-runtime container image
// from the source embedded in this binary. The build context
// (Dockerfile, entrypoint, outpost-cni source) ships with the outpost
// binary itself — no source checkout or external download required.
// Layers are cached by podman so reruns reuse the apt + curl + base
// image steps and only re-execute the lines whose inputs changed.
//
// Run this once after upgrading the outpost binary so the runtime
// container picks up entrypoint or CNI source changes (e.g. the
// apiserver→kubelet routing wiring landed in v0.1.2). Restart the
// runtime container afterwards: easiest path is `outpost restart`,
// which causes the daemon to tear down and re-launch its supervised
// container with the new image.
func clusterBuildRuntimeCmd() *cobra.Command {
	var (
		tag  string
		arch string
	)
	cmd := &cobra.Command{
		Use:   "build-runtime",
		Short: "Build the outpost-runtime container image from embedded sources",
		Long: `Materialize the embedded Dockerfile + entrypoint + outpost-cni
source to a tempdir and drive ` + "`podman build`" + ` to produce the
local outpost-runtime image the DKS agent supervisor expects.

Caching: base images (` + "`golang:1.25-alpine`, `debian:trixie-slim`" + `)
are cached in podman's local image store — important because Docker
Hub rate-limits anonymous pulls. Intermediate RUN layers are NOT
cached by ycode podman (the engine doesn't surface buildah's
` + "`--layers`" + ` flag), so every rebuild re-executes the apt +
curl steps. That's ~130 MB of network from non-rate-limited sources
(GitHub releases, Tailscale CDN, Debian mirrors) — annoying but not
blocking. Time to first-good-image is ~25 s on a fresh host.

To force a clean rebuild including base images, remove them first
(` + "`podman image rm outpost-runtime:dev golang:1.25-alpine debian:trixie-slim`" + `)
then re-run.

Examples:
  outpost cluster build-runtime
  outpost cluster build-runtime --tag outpost-runtime:next
  outpost cluster build-runtime --arch amd64        # cross-build for an amd64 host
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ref, err := runtime.BuildImage(cmd.Context(), runtime.BuildOptions{
				Tag:        tag,
				TargetArch: arch,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "built %s — restart the runtime container (e.g. `outpost restart`) so the supervisor picks it up.\n", ref)
			return nil
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "", `image reference to produce (default: outpost-runtime:dev)`)
	cmd.Flags().StringVar(&arch, "arch", "", `linux/* target arch ("amd64"|"arm64"); empty = host arch`)
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
				fmt.Println("Cleared. Restarting outpost — cluster runtime (k3s-agent or vkpodman) will stop on the next boot.")
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
				parsed, err := vknode.FetchKubeconfig(cmd.Context(), cloudboxBase, fc.AccessToken, node)
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

// clusterUserKubeconfigCmd fetches a per-USER kubeconfig from cloudbox
// (distinct from the per-host agent kubeconfig clusterKubeconfigCmd
// produces) and, by default, merges it into the operator's existing
// kubectl config at $KUBECONFIG / $HOME/.kube/config. The merge
// preserves any other contexts already there; only the cloudbox
// entries (stable names) get refreshed in place.
//
// The token cloudbox mints lives in the outpost-users namespace with
// RBAC scoped to the operator's per-user workload namespace. Lifetime
// is 1h — re-run before expiry (or wrap in cron / launchd timer for
// hands-off refresh).
//
// Use:
//
//	outpost cluster userkubeconfig                  # merge into ~/.kube/config
//	outpost cluster userkubeconfig --output ~/cloudbox.yaml   # standalone
//	outpost cluster userkubeconfig --stdout > /tmp/k         # print only
func clusterUserKubeconfigCmd() *cobra.Command {
	var (
		outputFlag string
		stdoutFlag bool
	)
	cmd := &cobra.Command{
		Use:   "userkubeconfig",
		Short: "Refresh the per-user kubectl config from cloudbox (merges into ~/.kube/config by default)",
		Long: `Fetch a fresh per-user kubectl kubeconfig from cloudbox and
merge it into the local kubectl config (~/.kube/config, or the first
entry of $KUBECONFIG).

Cloudbox identifies the calling account via the outpost's persisted
access_token (the same Bearer used for /api/cluster/agent) and mints a
per-user ServiceAccount token bound to the operator's workload
namespace. Token lifetime is 1 hour; re-run before expiry to refresh.

By default the operation is a kubectl-style merge — only the cloudbox
cluster/user/context entries get overwritten, and any other contexts
already in the kubeconfig stay intact. Use --output to write a
standalone YAML file instead (useful when you want to KUBECONFIG=
chain it without touching the default), or --stdout to print.`,
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
			cloudboxBase := cloudboxHTTPBase(fc)
			if cloudboxBase == "" {
				return errors.New("no cloudbox URL in saved config (server_addr / protocol missing)")
			}
			yaml, err := userkube.FetchUserKubeconfigYAML(cmd.Context(), cloudboxBase, fc.AccessToken)
			if err != nil {
				return err
			}
			if stdoutFlag {
				_, err := os.Stdout.Write(yaml)
				return err
			}
			if outputFlag != "" {
				if err := userkube.WriteStandalone(yaml, outputFlag); err != nil {
					return err
				}
				fmt.Printf("wrote %s\n", outputFlag)
				fmt.Println("Use it: export KUBECONFIG=" + outputFlag)
				return nil
			}
			path, err := userkube.MergeIntoKubectl(yaml, "")
			if err != nil {
				return err
			}
			fmt.Printf("merged into %s\n", path)
			fmt.Println("kubectl / helm should now work — token good for 1h, re-run to refresh")
			return nil
		},
	}
	cmd.Flags().StringVar(&outputFlag, "output", "",
		"Write a standalone YAML kubeconfig to this path (skips the merge)")
	cmd.Flags().BoolVar(&stdoutFlag, "stdout", false,
		"Print the kubeconfig YAML to stdout (skips the merge)")
	return cmd
}
