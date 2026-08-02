package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// `outpost cluster join <endpoint> --token …` — the worker half of
// control-plane placement.
//
// It shares a verb with the node-lifecycle `outpost cluster join` (no args:
// re-enable cluster mode against whichever plane is already configured)
// because they are the same operator intent at two levels of detail. Passing
// an endpoint says WHICH plane; passing nothing says "the one I already have".
//
// SECRETS NEVER COME FROM ARGV BY NECESSITY. --token is accepted because
// operators expect it, but --token-stdin and the environment variables exist
// so a scripted join never puts a credential in the process table or the shell
// history. Nothing here logs a token; the status printer only reports presence.

// Environment fallbacks for the three credentials, so a non-interactive join
// can avoid argv entirely.
const (
	envJoinToken  = "OUTPOST_CLUSTER_JOIN_TOKEN"
	envSTCPSecret = "OUTPOST_CLUSTER_STCP_SECRET"
	envNodeToken  = "OUTPOST_CLUSTER_NODE_TOKEN"
	envVKBundle   = "OUTPOST_CLUSTER_VK_BUNDLE"
)

// peerPlaneOut mirrors admincore.PeerPlaneResult over MCP. It has no field
// that could carry a credential — by construction, not by discipline.
type peerPlaneOut struct {
	OK             bool   `json:"ok"`
	Joined         bool   `json:"joined"`
	Endpoint       string `json:"endpoint,omitempty"`
	APIPort        int    `json:"api_port,omitempty"`
	HasToken       bool   `json:"has_token"`
	HasSTCPSecret  bool   `json:"has_stcp_secret"`
	HasNodeToken   bool   `json:"has_node_token"`
	ClusterEnabled bool   `json:"cluster_enabled"`
	RestartPending bool   `json:"restart_pending"`

	HasVKCredential   bool     `json:"has_vk_credential"`
	VKCredentialKind  string   `json:"vk_credential_kind,omitempty"`
	AllowedNamespaces []string `json:"allowed_namespaces,omitempty"`

	RuntimeAgent   bool     `json:"runtime_agent"`
	RuntimeVirtual []string `json:"runtime_virtual,omitempty"`
}

// clusterJoinFlags is the flag set shared by the MCP and --offline paths.
type clusterJoinFlags struct {
	token      string
	tokenStdin bool
	stcpSecret string
	nodeToken  string
	vkBundle   string
	apiPort    int
	offline    bool
	show       bool
	clear      bool

	// clusterAgent is on|off|"" — the tri-state parseToggle wants, so an
	// unspecified flag stays distinguishable from --cluster-agent=off.
	clusterAgent   string
	clusterVirtual []string
}

// resolveJoinParams turns flags + env + stdin into the partial update. Only
// fields the operator actually supplied are non-nil, so re-running the command
// with a rotated token leaves the other credentials in place.
func resolveJoinParams(cmd *cobra.Command, endpoint string, f *clusterJoinFlags) (admincore.PeerPlaneParams, error) {
	var p admincore.PeerPlaneParams
	if endpoint != "" {
		ep := endpoint
		p.Endpoint = &ep
	}

	token := f.token
	if f.tokenStdin {
		if token != "" {
			return p, errors.New("pass either --token or --token-stdin, not both")
		}
		b, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return p, fmt.Errorf("read token from stdin: %w", err)
		}
		token = strings.TrimSpace(string(b))
		if token == "" {
			return p, errors.New("--token-stdin: stdin was empty")
		}
	}
	if token == "" {
		token = strings.TrimSpace(os.Getenv(envJoinToken))
	}
	if token != "" {
		p.Token = &token
	}

	if s := firstNonEmpty(f.stcpSecret, os.Getenv(envSTCPSecret)); s != "" {
		p.STCPSecret = &s
	}
	if s := firstNonEmpty(f.nodeToken, os.Getenv(envNodeToken)); s != "" {
		p.NodeToken = &s
	}
	if s := firstNonEmpty(f.vkBundle, os.Getenv(envVKBundle)); s != "" {
		p.VKBundle = &s
	}
	if cmd.Flags().Changed("api-port") {
		port := f.apiPort
		p.APIPort = &port
	}

	// Runtime selection. Both stay nil unless the operator named them, which
	// is what keeps a plain `outpost cluster join <endpoint>` on the
	// agent-only default it has always produced.
	agent, err := parseToggle("cluster-agent", f.clusterAgent)
	if err != nil {
		return p, err
	}
	p.Agent = agent
	if cmd.Flags().Changed("cluster-virtual") {
		// Validate here too so a typo fails before a daemon round-trip, and
		// with the same rule admincore applies.
		if _, err := conf.NormalizeVirtualRuntimes(f.clusterVirtual); err != nil {
			return p, fmt.Errorf("--cluster-virtual: %w", err)
		}
		// A non-nil empty slice is meaningful (deselect every backend), so
		// normalize the nil that StringSliceVar hands back for `--cluster-virtual=`.
		p.Virtual = f.clusterVirtual
		if p.Virtual == nil {
			p.Virtual = []string{}
		}
	}
	return p, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// wantsPeerJoin reports whether the invocation names a peer plane, as opposed
// to being the bare node-lifecycle rejoin.
func wantsPeerJoin(endpoint string, f *clusterJoinFlags) bool {
	return endpoint != "" || f.token != "" || f.tokenStdin ||
		f.stcpSecret != "" || f.nodeToken != "" || f.vkBundle != "" ||
		f.clusterAgent != "" || f.clusterVirtual != nil ||
		os.Getenv(envJoinToken) != ""
}

func runPeerJoin(ctx context.Context, p admincore.PeerPlaneParams, offline bool) error {
	if offline {
		core, err := offlineCore()
		if err != nil {
			return err
		}
		res, err := core.JoinPeerPlane(p)
		if err != nil {
			return err
		}
		printPeerPlane(peerPlaneOut{
			OK: res.OK, Joined: res.Joined, Endpoint: res.Endpoint, APIPort: res.APIPort,
			HasToken: res.HasToken, HasSTCPSecret: res.HasSTCPSecret, HasNodeToken: res.HasNodeToken,
			HasVKCredential: res.HasVKCredential, VKCredentialKind: res.VKCredentialKind,
			AllowedNamespaces: res.AllowedNamespaces,
			ClusterEnabled:    res.ClusterEnabled,
			RuntimeAgent:      res.RuntimeAgent, RuntimeVirtual: res.RuntimeVirtual,
		})
		fmt.Println("\nWritten to agent.json. Start (or restart) outpost to join.")
		return nil
	}

	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	var out peerPlaneOut
	if err := session.callTool(ctx, "outpost_cluster_join_peer", peerJoinArgs(p), &out); err != nil {
		return err
	}
	printPeerPlane(out)
	if out.RestartPending {
		if err := restartViaMCP(ctx); err != nil {
			return fmt.Errorf("join persisted, but the restart that applies it failed (run `outpost restart`): %w", err)
		}
		fmt.Println("\nRestarting outpost to join. Poll `outpost status`, then `kubectl get nodes` on the control plane.")
	}
	return nil
}

// peerJoinArgs builds the MCP argument map, omitting anything the operator
// didn't supply so the daemon's "nil means leave alone" contract holds.
func peerJoinArgs(p admincore.PeerPlaneParams) map[string]any {
	args := map[string]any{}
	if p.Endpoint != nil {
		args["endpoint"] = *p.Endpoint
	}
	if p.Token != nil {
		args["token"] = *p.Token
	}
	if p.STCPSecret != nil {
		args["stcp_secret"] = *p.STCPSecret
	}
	if p.NodeToken != nil {
		args["node_token"] = *p.NodeToken
	}
	if p.VKBundle != nil {
		args["vk_bundle"] = *p.VKBundle
	}
	if p.APIPort != nil {
		args["api_port"] = *p.APIPort
	}
	if p.Agent != nil {
		args["cluster_agent"] = *p.Agent
	}
	if p.Virtual != nil {
		args["cluster_virtual"] = p.Virtual
	}
	return args
}

func runPeerPlaneShow(ctx context.Context, offline bool) error {
	if offline {
		core, err := offlineCore()
		if err != nil {
			return err
		}
		res, err := core.PeerPlaneView()
		if err != nil {
			return err
		}
		printPeerPlane(peerPlaneOut{
			OK: res.OK, Joined: res.Joined, Endpoint: res.Endpoint, APIPort: res.APIPort,
			HasToken: res.HasToken, HasSTCPSecret: res.HasSTCPSecret, HasNodeToken: res.HasNodeToken,
			HasVKCredential: res.HasVKCredential, VKCredentialKind: res.VKCredentialKind,
			AllowedNamespaces: res.AllowedNamespaces,
			ClusterEnabled:    res.ClusterEnabled,
			RuntimeAgent:      res.RuntimeAgent, RuntimeVirtual: res.RuntimeVirtual,
		})
		return nil
	}
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	var out peerPlaneOut
	if err := session.callTool(ctx, "outpost_cluster_peer_plane", map[string]any{}, &out); err != nil {
		return err
	}
	printPeerPlane(out)
	return nil
}

func runPeerPlaneClear(ctx context.Context, offline bool) error {
	if offline {
		core, err := offlineCore()
		if err != nil {
			return err
		}
		if _, err := core.LeavePeerPlane(); err != nil {
			return err
		}
		fmt.Println("Cleared — this host now joins the cloudbox-hosted plane. Restart outpost to apply.")
		return nil
	}
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	var out peerPlaneOut
	if err := session.callTool(ctx, "outpost_cluster_leave_peer", map[string]any{}, &out); err != nil {
		return err
	}
	fmt.Println("Cleared — this host now joins the cloudbox-hosted plane.")
	if out.RestartPending {
		if err := restartViaMCP(ctx); err != nil {
			return fmt.Errorf("cleared, but the restart that applies it failed (run `outpost restart`): %w", err)
		}
		fmt.Println("Restarting outpost to apply.")
	}
	return nil
}

// printPeerPlane renders the redacted view. Credentials appear as presence
// only — this output is the kind that ends up in a screenshare.
func printPeerPlane(out peerPlaneOut) {
	if !out.Joined {
		fmt.Println("control plane: cloudbox-hosted (no peer endpoint configured)")
	} else {
		fmt.Printf("control plane: peer-hosted at %s\n", out.Endpoint)
	}
	fmt.Printf("join token:    %s\n", presence(out.HasToken))
	fmt.Printf("stcp secret:   %s\n", presence(out.HasSTCPSecret))
	fmt.Printf("node token:    %s\n", presence(out.HasNodeToken))
	if out.HasVKCredential {
		vk := "set"
		if out.VKCredentialKind != "" {
			vk = "set (" + out.VKCredentialKind + ")"
		}
		fmt.Printf("vk credential: %s\n", vk)
	} else {
		fmt.Println("vk credential: missing")
	}
	if len(out.AllowedNamespaces) > 0 {
		fmt.Printf("vk namespaces: %s\n", strings.Join(out.AllowedNamespaces, ", "))
	}
	if out.APIPort != 0 {
		fmt.Printf("apiserver:     127.0.0.1:%d (bound locally by this worker)\n", out.APIPort)
	}
	fmt.Printf("cluster mode:  %s\n", onOff(out.ClusterEnabled))
	fmt.Printf("runtimes:      %s\n", runtimeSummary(out.RuntimeAgent, out.RuntimeVirtual))
	if out.Joined && !(out.HasToken && out.HasSTCPSecret && out.HasNodeToken) {
		// A worker missing one of the three authenticates and then fails to
		// reach the apiserver — a failure that reads like a broken network.
		fmt.Println("\nNOTE: a peer join needs all three credentials. Read them on the hosting machine:")
		fmt.Println("  outpost cluster control-plane token   # endpoint + join token + stcp secret")
		fmt.Println("  outpost cluster token                 # node token")
	}
	if out.Joined && len(out.RuntimeVirtual) > 0 && !out.HasVKCredential {
		// Reachable only from the status view — the join itself refuses this
		// state — but a config written before the vk bundle existed can show it.
		fmt.Println("\nNOTE: virtual runtimes are selected but no vk credential is set — every vk node")
		fmt.Println("will fail to authenticate. Mint one on the hosting machine and re-join:")
		fmt.Println("  outpost cluster control-plane vk-credential   # bundle for --vk-bundle")
	}
}

// runtimeSummary renders which Nodes this worker registers, naming the
// agent runtime explicitly rather than leaving it implied — the whole point of
// the selection is that "agent" is now one choice among several.
func runtimeSummary(agent bool, virtual []string) string {
	modes := make([]string, 0, 1+len(virtual))
	if agent {
		modes = append(modes, conf.ClusterRuntimeAgent)
	}
	modes = append(modes, virtual...)
	if len(modes) == 0 {
		return "none selected"
	}
	return strings.Join(modes, ", ")
}

func presence(has bool) string {
	if has {
		return "set"
	}
	return "missing"
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
