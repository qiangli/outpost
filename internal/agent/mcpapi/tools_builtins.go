package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// builtinsIn mirrors admincore.BuiltinsParams. Pointer-bool fields
// mean "leave unchanged when omitted"; agent tools populate only
// what they intend to change.
type builtinsIn struct {
	Shell                  *bool               `json:"shell,omitempty" jsonschema:"Toggle the /shell PTY route"`
	Desktop                *bool               `json:"desktop,omitempty" jsonschema:"Toggle the /desktop VNC route"`
	Clipboard              *bool               `json:"clipboard,omitempty" jsonschema:"Toggle the /clipboard pbcopy/pbpaste route"`
	SSH                    *bool               `json:"ssh,omitempty" jsonschema:"Toggle the /ssh built-in SSH server"`
	SSHAllowLocalForward   *bool               `json:"ssh_allow_local_forward,omitempty" jsonschema:"Allow direct-tcpip channels (ssh -L); loopback dest only"`
	SSHAllowRemoteForward  *bool               `json:"ssh_allow_remote_forward,omitempty" jsonschema:"Allow tcpip-forward global requests (ssh -R); loopback bind only"`
	SSHAllowAgentForward   *bool               `json:"ssh_allow_agent_forward,omitempty" jsonschema:"Allow auth-agent-req channels (ssh -A)"`
	SSHForwardSockets      []string            `json:"ssh_forward_sockets,omitempty" jsonschema:"Additional unix-socket paths to allow for direct-streamlocal forwards"`
	SFTP                   *bool               `json:"sftp,omitempty" jsonschema:"Toggle the sftp subsystem (scp 8.8+ uses sftp)"`
	Files                  *bool               `json:"files,omitempty" jsonschema:"Toggle the built-in File Browser (embedded; GUI for remote view/download)"`
	FilesAllowWrite        *bool               `json:"files_allow_write,omitempty" jsonschema:"Allow write ops in File Browser (upload/edit/rename/delete); default off = read-only + download-only. Loopback-admin-plane only — this is the LAN-gated control."`
	FilesScope             *string             `json:"files_scope,omitempty" jsonschema:"Root directory File Browser is confined to (empty = the OS user's home)"`
	Podman                 *bool               `json:"podman,omitempty" jsonschema:"Toggle the raw (admin-only) built-in podman passthrough proxy. DEFAULT ON (omit = leave as-is; absent config key = on). The mount is only registered when a podman socket is actually detected — a host without podman logs and carries on. Set false to opt out; the opt-out is persisted explicitly and survives later writes."`
	Sandbox                *bool               `json:"sandbox,omitempty" jsonschema:"Toggle the filtered container sandbox proxy (strips privileged/host-ns/host-binds/caps/devices, injects resource caps; needs podman)"`
	Ollama                 *bool               `json:"ollama,omitempty" jsonschema:"Toggle the built-in ollama proxy. DEFAULT ON (absent config key = on); registered only when an Ollama daemon is detected, otherwise skipped with a log line. Set false to opt out."`
	OllamaPool             *bool               `json:"ollama_pool,omitempty" jsonschema:"Participate in cloudbox's multi-host LLM pool (requires Ollama on)"`
	WarmServing            *bool               `json:"warm_serving,omitempty" jsonschema:"Toggle the adaptive, considerate warm-serving plane: keep a small conservative set of models resident (zero cold-start), yielding (unloading) whenever the host is busy with the user's own work and restoring when idle. Default ON for a paired Ollama node."`
	WarmBudgetFrac         *float64            `json:"warm_budget_frac,omitempty" jsonschema:"Fraction of usable memory dedicated to warm preload (clamped to (0,1]; default 0.33 — leaves ~2/3 for the OS and the user's apps). The budget drops to 0 whenever the host is busy."`
	Otel                   *bool               `json:"otel,omitempty" jsonschema:"Expose the local embedded observability stack (Prom/Alertmanager/VictoriaLogs/Jaeger/Perses) as built-in apps. DEFAULT ON (absent config key = on); the surfaces are only registered when a local observability proxy is detected, otherwise skipped with a log line. Each surface still requires per-app login elevation. Set false to opt out."`
	OtelPool               *bool               `json:"otel_pool,omitempty" jsonschema:"Allow cloudbox to federate queries / fan-out alert rules across this host's observability stack (requires Otel on)"`
	YcodeShare             *bool               `json:"ycode_share,omitempty" jsonschema:"Expose ycode-backed UI surfaces through the matrix tunnel (requires Ycode on; default on)"`
	YcodeShareRequireLogin *bool               `json:"ycode_share_require_login,omitempty" jsonschema:"Require cloudbox-side OS-password elevation for the ycode-* built-in apps (default off; on = OS password popup like /shell or /desktop)"`
	YcodeShareSurfaces     map[string]bool     `json:"ycode_share_surfaces,omitempty" jsonschema:"Per-surface opt-in overlay for the ycode-share catalog. Keys: ycode, ycode-canvas, ycode-ollama, ycode-git, ycode-memos, ycode-graph. Absent keys fall back to catalog defaults (only 'ycode' is default-on). Partial maps are merged into the persisted overlay; not a full replace."`
	Cluster                *bool               `json:"cluster,omitempty" jsonschema:"Join the cloudbox virtual-podman cluster as a node. OPT-IN (default off), unlike the podman/ollama/otel proxies: joining lets a remote control plane schedule workloads on this host and starts a k3s-agent (or virtual-kubelet) runtime, so it must be chosen explicitly."`
	ClusterMode            *string             `json:"cluster_mode,omitempty" jsonschema:"How this host runs cluster workloads — one of 'agent' (real k3s-agent in the outpost-runtime container; the default and the conformance track), 'vk-podman' (v1 virtual-kubelet shim landing Pods as local podman containers; alias 'vkpodman'), 'vk-native' (virtual-kubelet landing Pods as host processes), or 'vk-ollama' (legacy native-process mode for Metal/CUDA workloads)"`
	UpdateMode             *string             `json:"update_mode,omitempty" jsonschema:"Per-host policy for cloudbox-pushed self-upgrades — one of 'auto' (default; stage+swap+restart on push), 'manual' (persist envelope, operator applies), 'never' (refuse)"`
	AutoRollback           *bool               `json:"auto_rollback,omitempty" jsonschema:"Arm the auto-rollback watchdog's destructive revert: when a self-upgrade's new binary fails to confirm healthy, the supervisor reverts to the previous binary. Default off (observe-only — logs 'would auto-rollback')."`
	Mesh                   *bool               `json:"mesh,omitempty" jsonschema:"Toggle the libp2p mesh data plane (the peer node carrying authenticated, NAT-traversing peer↔peer streams — transport under shard-RPC, peer-backup, the resource fabric). Requires a paired access token."`
	MeshPort               *int                `json:"mesh_port,omitempty" jsonschema:"TCP+QUIC listen port for the mesh host (0 = ephemeral; a stable port helps NAT/hole-punch)"`
	LANInference           *bool               `json:"lan_inference,omitempty" jsonschema:"Toggle the same-LAN direct-inference listener: a LAN-reachable reverse proxy to the local inference server (Ollama / shard leader), advertised to cloudbox so same-LAN callers reach this host's LLM directly (lower latency, bypassing the relay). LAN-TRUST endpoint: NOT authenticated per-request — only enable on a trusted LAN. Requires Ollama on + pairing. Default off."`
	LANInferencePort       *int                `json:"lan_inference_port,omitempty" jsonschema:"TCP port the LAN inference listener binds on all interfaces (0 = default 11435; must differ from the inference server's 11434)"`
	Loom                   *bool               `json:"loom,omitempty" jsonschema:"Toggle running the loom git forge (Gitea) as a managed external binary on a loopback port, auto-exposed over the mesh as the 'git' service. Downloaded/verified/cached by binmgr — not compiled in."`
	LoomPort               *int                `json:"loom_port,omitempty" jsonschema:"loom's loopback HTTP port (0 = default 31880)"`
	BashyServices          []conf.BashyService `json:"bashy_services,omitempty" jsonschema:"Generic bashy-managed services. Each enabled row is supervised by outpost through 'bashy <name> start|status|stop' and may publish a cloudbox app and mesh service."`
	BashyVersion           *string             `json:"bashy_version,omitempty" jsonschema:"Pin the bashy release the daemon auto-installs when bashy is missing (empty/'latest' = newest; e.g. v0.3.0). Pin in production so a restart can't silently pull a new bashy."`
	Zot                    *bool               `json:"zot,omitempty" jsonschema:"Toggle running the Zot OCI registry as a managed external binary on a loopback port, auto-exposed over the mesh as the 'registry' service (serves container images + Ollama models). Downloaded/verified/cached by binmgr — not compiled in."`
	ZotPort                *int                `json:"zot_port,omitempty" jsonschema:"zot's loopback HTTP port (0 = default 5000)"`
	Seaweedfs              *bool               `json:"seaweedfs,omitempty" jsonschema:"Toggle running SeaweedFS (object/blob store, S3 gateway) as a managed external binary on a loopback port, auto-exposed over the mesh as the 's3' service. Can also back zot's blob store. Downloaded/verified/cached by binmgr — not compiled in."`
	SeaweedfsPort          *int                `json:"seaweedfs_port,omitempty" jsonschema:"SeaweedFS's loopback S3-gateway port (0 = default 8333)"`
	Kopia                  *bool               `json:"kopia,omitempty" jsonschema:"Toggle running the Kopia snapshot-backup repository server as a managed external binary on a loopback port, auto-exposed over the mesh as the 'backup' service (many nodes back up into one repo). Downloaded/verified/cached by binmgr — not compiled in."`
	KopiaPort              *int                `json:"kopia_port,omitempty" jsonschema:"Kopia's loopback server port (0 = default 51515)"`
	Actrunner              *bool               `json:"actrunner,omitempty" jsonschema:"Toggle running Gitea act_runner (the CI executor) as a managed external binary. A CONSUMER (registers against a Gitea instance and dials out), not a mesh service. Downloaded/verified/cached by binmgr — not compiled in."`
	ActrunnerInstance      *string             `json:"actrunner_instance,omitempty" jsonschema:"Gitea base URL the runner registers against (empty = local loom forge)"`
	ActrunnerToken         *string             `json:"actrunner_token,omitempty" jsonschema:"act_runner registration token (minted in Gitea)"`
	ActrunnerLabels        *string             `json:"actrunner_labels,omitempty" jsonschema:"executor labels (default 'host:host')"`
	CloudDOEnabled         *bool               `json:"cloud_do_enabled,omitempty" jsonschema:"Toggle Digital Ocean provider support (the cloud venue): advertises DO capability and exports the token as DIGITALOCEAN_ACCESS_TOKEN for bashy doctl / DO provisioning. Off by default."`
	CloudDOToken           *string             `json:"cloud_do_token,omitempty" jsonschema:"Digital Ocean API token (exported as DIGITALOCEAN_ACCESS_TOKEN; redacted from status reads)"`
	ActrunnerSandbox       *bool               `json:"actrunner_sandbox,omitempty" jsonschema:"Opt the runner into the tier-3 sandbox (container) executor: adds a 'sandbox' docker-executor label so runs-on:sandbox jobs run in an OCI container via bashy podman. Additive to the host build lane."`
	ActrunnerSandboxImage  *string             `json:"actrunner_sandbox_image,omitempty" jsonschema:"OCI image the sandbox executor runs jobs in (empty = a node image with git+node+bash)"`
	ActrunnerDockerHost    *string             `json:"actrunner_docker_host,omitempty" jsonschema:"DOCKER_HOST the sandbox executor dials (empty = auto-resolve bashy podman's socket)"`
	Shard                  *bool               `json:"shard,omitempty" jsonschema:"Toggle Ollama sharding — serve a model bigger than one node by splitting it across same-LAN mesh peers. Default ON for a paired Ollama node; set false to opt out."`
	ShardPeers             []string            `json:"shard_peers,omitempty" jsonschema:"Worker hostnames for the shard ring (empty or ['auto'] = every reachable same-LAN peer)"`
	ShardRole              *string             `json:"shard_role,omitempty" jsonschema:"Shard role: 'auto' (most-VRAM host leads), 'leader', or 'worker'"`
}

type setBuiltinsOut struct {
	OK             bool `json:"ok"`
	RestartPending bool `json:"restart_pending"`
}

func (s *Server) registerBuiltinsTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_set_builtins",
		Description: "Toggle built-in routes (shell/desktop/clipboard/ssh/sftp/files), the SSH-channel allow flags, the File Browser scope + write switch, and the local-daemon proxies (podman/ollama/otel are default-ON and detection-gated; the container sandbox, LLM pool and cluster join are opt-ins). Only fields present in the call are modified.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in builtinsIn) (*mcp.CallToolResult, setBuiltinsOut, error) {
		res, err := s.core.SetBuiltins(admincore.BuiltinsParams{
			Shell:                  in.Shell,
			Desktop:                in.Desktop,
			Clipboard:              in.Clipboard,
			SSH:                    in.SSH,
			SSHAllowLocalForward:   in.SSHAllowLocalForward,
			SSHAllowRemoteForward:  in.SSHAllowRemoteForward,
			SSHAllowAgentForward:   in.SSHAllowAgentForward,
			SSHForwardSockets:      in.SSHForwardSockets,
			SFTP:                   in.SFTP,
			Files:                  in.Files,
			FilesAllowWrite:        in.FilesAllowWrite,
			FilesScope:             in.FilesScope,
			Podman:                 in.Podman,
			Sandbox:                in.Sandbox,
			Ollama:                 in.Ollama,
			OllamaPool:             in.OllamaPool,
			WarmServing:            in.WarmServing,
			WarmBudgetFrac:         in.WarmBudgetFrac,
			Otel:                   in.Otel,
			OtelPool:               in.OtelPool,
			YcodeShare:             in.YcodeShare,
			YcodeShareRequireLogin: in.YcodeShareRequireLogin,
			YcodeShareSurfaces:     in.YcodeShareSurfaces,
			Cluster:                in.Cluster,
			ClusterMode:            in.ClusterMode,
			UpdateMode:             in.UpdateMode,
			AutoRollback:           in.AutoRollback,
			Mesh:                   in.Mesh,
			MeshPort:               in.MeshPort,
			LANInference:           in.LANInference,
			LANInferencePort:       in.LANInferencePort,
			Loom:                   in.Loom,
			LoomPort:               in.LoomPort,
			BashyServices:          in.BashyServices,
			BashyVersion:           in.BashyVersion,
			Zot:                    in.Zot,
			ZotPort:                in.ZotPort,
			Seaweedfs:              in.Seaweedfs,
			SeaweedfsPort:          in.SeaweedfsPort,
			Kopia:                  in.Kopia,
			KopiaPort:              in.KopiaPort,
			Actrunner:              in.Actrunner,
			ActrunnerInstance:      in.ActrunnerInstance,
			ActrunnerToken:         in.ActrunnerToken,
			ActrunnerLabels:        in.ActrunnerLabels,
			CloudDOEnabled:         in.CloudDOEnabled,
			CloudDOToken:           in.CloudDOToken,
			ActrunnerSandbox:       in.ActrunnerSandbox,
			ActrunnerSandboxImage:  in.ActrunnerSandboxImage,
			ActrunnerDockerHost:    in.ActrunnerDockerHost,
			Shard:                  in.Shard,
			ShardPeers:             in.ShardPeers,
			ShardRole:              in.ShardRole,
		})
		if err != nil {
			return apiErrResult[setBuiltinsOut](err)
		}
		return nil, setBuiltinsOut{OK: res.OK, RestartPending: res.RestartPending}, nil
	})
}
