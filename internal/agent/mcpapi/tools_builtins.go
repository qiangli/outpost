package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// builtinsIn mirrors admincore.BuiltinsParams. Pointer-bool fields
// mean "leave unchanged when omitted"; agent tools populate only
// what they intend to change.
type builtinsIn struct {
	Shell                 *bool    `json:"shell,omitempty" jsonschema:"Toggle the /shell PTY route"`
	Desktop               *bool    `json:"desktop,omitempty" jsonschema:"Toggle the /desktop VNC route"`
	Clipboard             *bool    `json:"clipboard,omitempty" jsonschema:"Toggle the /clipboard pbcopy/pbpaste route"`
	SSH                   *bool    `json:"ssh,omitempty" jsonschema:"Toggle the /ssh built-in SSH server"`
	SSHAllowLocalForward  *bool    `json:"ssh_allow_local_forward,omitempty" jsonschema:"Allow direct-tcpip channels (ssh -L); loopback dest only"`
	SSHAllowRemoteForward *bool    `json:"ssh_allow_remote_forward,omitempty" jsonschema:"Allow tcpip-forward global requests (ssh -R); loopback bind only"`
	SSHAllowAgentForward  *bool    `json:"ssh_allow_agent_forward,omitempty" jsonschema:"Allow auth-agent-req channels (ssh -A)"`
	SSHForwardSockets     []string `json:"ssh_forward_sockets,omitempty" jsonschema:"Additional unix-socket paths to allow for direct-streamlocal forwards"`
	SFTP                  *bool    `json:"sftp,omitempty" jsonschema:"Toggle the sftp subsystem (scp 8.8+ uses sftp)"`
	// files / podman / sandbox / ollama / ollama_pool / warm_serving /
	// warm_budget_frac / otel / otel_pool / ycode_share* / lan_inference* /
	// shard* are GONE from this tool. Each is a service unit now, so an
	// agent switches it with `services` and configures it with
	// `service_settings` — the same two args a seeded built-in or a
	// third-party daemon takes. Read outpost://services for the unit names,
	// their capability ids and their settings.
	Cluster         *bool                        `json:"cluster,omitempty" jsonschema:"Join the cloudbox virtual-podman cluster as a node. OPT-IN (default off), unlike the podman/ollama/otel proxies: joining lets a remote control plane schedule workloads on this host and starts a k3s-agent (or virtual-kubelet) runtime, so it must be chosen explicitly."`
	ClusterAgent    *bool                        `json:"cluster_agent,omitempty" jsonschema:"Run one real k3s-agent Node on this host."`
	ClusterVirtual  []string                     `json:"cluster_virtual,omitempty" jsonschema:"Complete set of virtual-kubelet backends to run concurrently: vk-podman, vk-native, vk-ollama."`
	UpdateMode      *string                      `json:"update_mode,omitempty" jsonschema:"Per-host policy for cloudbox-pushed self-upgrades — one of 'auto' (default; stage+swap+restart on push), 'manual' (persist envelope, operator applies), 'never' (refuse)"`
	Mesh            *bool                        `json:"mesh,omitempty" jsonschema:"Toggle the libp2p mesh data plane (the peer node carrying authenticated, NAT-traversing peer↔peer streams — transport under shard-RPC, peer-backup, the resource fabric). Requires a paired access token."`
	MeshPort        *int                         `json:"mesh_port,omitempty" jsonschema:"TCP+QUIC listen port for the mesh host (0 = ephemeral; a stable port helps NAT/hole-punch)"`
	Services        map[string]bool              `json:"services,omitempty" jsonschema:"Desired state of service units, by unit name: {\"meet\":true,\"loom\":false}. This is the one way to turn a service on or off — the same write 'bashy service enable|disable' performs — and it applies LIVE: the monitor reconciles the process and outpost republishes the unit's declared app/mesh exposure without an outpost restart. Units are declared YAML (list them with 'bashy service list'), so a third-party daemon is toggled here with no outpost change."`
	ServiceSettings map[string]map[string]string `json:"service_settings,omitempty" jsonschema:"Declaration overrides per unit: {\"loom\":{\"ports.0.port\":\"31900\"}}. Dotted paths into the unit record, the 'bashy service set NAME --set k=v' shape. Overriding a seeded built-in writes a sparse local overlay; 'bashy service rm' restores the seed."`
	BashyVersion    *string                      `json:"bashy_version,omitempty" jsonschema:"Pin the bashy release the daemon auto-installs when bashy is missing (empty/'latest' = newest; e.g. v0.3.0). Pin in production so a restart can't silently pull a new bashy."`
	CloudDOEnabled  *bool                        `json:"cloud_do_enabled,omitempty" jsonschema:"Toggle Digital Ocean provider support (the cloud venue): advertises DO capability and exports the token as DIGITALOCEAN_ACCESS_TOKEN for bashy doctl / DO provisioning. Off by default."`
	CloudDOToken    *string                      `json:"cloud_do_token,omitempty" jsonschema:"Digital Ocean API token (exported as DIGITALOCEAN_ACCESS_TOKEN; redacted from status reads)"`
}

type setBuiltinsOut struct {
	OK             bool `json:"ok"`
	RestartPending bool `json:"restart_pending"`
}

func (s *Server) registerBuiltinsTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_set_builtins",
		Description: "Toggle the built-in routes (shell/desktop/clipboard/ssh/sftp), the SSH-channel allow flags, the mesh data plane, the cluster join and the upgrade policy — and the desired state of SERVICE UNITS via `services` plus their declaration overrides via `service_settings`. Everything outpost used to carry its own toggle for (the File Browser, the podman and sandbox container mounts, the Ollama proxy and its pool, the warm and shard planes, the same-LAN inference listener, the observability and ycode surfaces) is a unit: switch it through `services` and configure it through `service_settings`. Service writes apply LIVE (no restart); the remaining FileConfig toggles set restart_pending. Read the units first from the outpost://services resource. Only fields present in the call are modified.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in builtinsIn) (*mcp.CallToolResult, setBuiltinsOut, error) {
		res, err := s.core.SetBuiltins(ctx, admincore.BuiltinsParams{
			Shell:                 in.Shell,
			Desktop:               in.Desktop,
			Clipboard:             in.Clipboard,
			SSH:                   in.SSH,
			SSHAllowLocalForward:  in.SSHAllowLocalForward,
			SSHAllowRemoteForward: in.SSHAllowRemoteForward,
			SSHAllowAgentForward:  in.SSHAllowAgentForward,
			SSHForwardSockets:     in.SSHForwardSockets,
			SFTP:                  in.SFTP,
			Cluster:               in.Cluster,
			ClusterAgent:          in.ClusterAgent,
			ClusterVirtual:        in.ClusterVirtual,
			UpdateMode:            in.UpdateMode,
			Mesh:                  in.Mesh,
			MeshPort:              in.MeshPort,
			Services:              in.Services,
			ServiceSettings:       in.ServiceSettings,
			BashyVersion:          in.BashyVersion,
			CloudDOEnabled:        in.CloudDOEnabled,
			CloudDOToken:          in.CloudDOToken,
		})
		if err != nil {
			return apiErrResult[setBuiltinsOut](err)
		}
		return nil, setBuiltinsOut{OK: res.OK, RestartPending: res.RestartPending}, nil
	})
}
