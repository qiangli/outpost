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
	Shell                  *bool           `json:"shell,omitempty" jsonschema:"Toggle the /shell PTY route"`
	Desktop                *bool           `json:"desktop,omitempty" jsonschema:"Toggle the /desktop VNC route"`
	Clipboard              *bool           `json:"clipboard,omitempty" jsonschema:"Toggle the /clipboard pbcopy/pbpaste route"`
	SSH                    *bool           `json:"ssh,omitempty" jsonschema:"Toggle the /ssh built-in SSH server"`
	SSHAllowLocalForward   *bool           `json:"ssh_allow_local_forward,omitempty" jsonschema:"Allow direct-tcpip channels (ssh -L); loopback dest only"`
	SSHAllowRemoteForward  *bool           `json:"ssh_allow_remote_forward,omitempty" jsonschema:"Allow tcpip-forward global requests (ssh -R); loopback bind only"`
	SSHAllowAgentForward   *bool           `json:"ssh_allow_agent_forward,omitempty" jsonschema:"Allow auth-agent-req channels (ssh -A)"`
	SSHForwardSockets      []string        `json:"ssh_forward_sockets,omitempty" jsonschema:"Additional unix-socket paths to allow for direct-streamlocal forwards"`
	SFTP                   *bool           `json:"sftp,omitempty" jsonschema:"Toggle the sftp subsystem (scp 8.8+ uses sftp)"`
	Files                  *bool           `json:"files,omitempty" jsonschema:"Toggle the built-in File Browser (embedded; GUI for remote view/download)"`
	FilesAllowWrite        *bool           `json:"files_allow_write,omitempty" jsonschema:"Allow write ops in File Browser (upload/edit/rename/delete); default off = read-only + download-only. Loopback-admin-plane only — this is the LAN-gated control."`
	FilesScope             *string         `json:"files_scope,omitempty" jsonschema:"Root directory File Browser is confined to (empty = the OS user's home)"`
	Podman                 *bool           `json:"podman,omitempty" jsonschema:"Toggle the raw (admin-only) built-in podman passthrough proxy"`
	Sandbox                *bool           `json:"sandbox,omitempty" jsonschema:"Toggle the filtered container sandbox proxy (strips privileged/host-ns/host-binds/caps/devices, injects resource caps; needs podman)"`
	Ollama                 *bool           `json:"ollama,omitempty" jsonschema:"Toggle the built-in ollama proxy"`
	OllamaPool             *bool           `json:"ollama_pool,omitempty" jsonschema:"Participate in cloudbox's multi-host LLM pool (requires Ollama on)"`
	Otel                   *bool           `json:"otel,omitempty" jsonschema:"Expose ycode's embedded observability stack (Prom/Alertmanager/VictoriaLogs/Jaeger/Perses) as built-in apps"`
	OtelPool               *bool           `json:"otel_pool,omitempty" jsonschema:"Allow cloudbox to federate queries / fan-out alert rules across this host's observability stack (requires Otel on)"`
	YcodeShare             *bool           `json:"ycode_share,omitempty" jsonschema:"Expose ycode-backed UI surfaces through the matrix tunnel (requires Ycode on; default on)"`
	YcodeShareRequireLogin *bool           `json:"ycode_share_require_login,omitempty" jsonschema:"Require cloudbox-side OS-password elevation for the ycode-* built-in apps (default off; on = OS password popup like /shell or /desktop)"`
	YcodeShareSurfaces     map[string]bool `json:"ycode_share_surfaces,omitempty" jsonschema:"Per-surface opt-in overlay for the ycode-share catalog. Keys: ycode, ycode-canvas, ycode-ollama, ycode-git, ycode-memos, ycode-graph. Absent keys fall back to catalog defaults (only 'ycode' is default-on). Partial maps are merged into the persisted overlay; not a full replace."`
	Cluster                *bool           `json:"cluster,omitempty" jsonschema:"Join the cloudbox virtual-podman cluster as a node"`
	UpdateMode             *string         `json:"update_mode,omitempty" jsonschema:"Per-host policy for cloudbox-pushed self-upgrades — one of 'auto' (default; stage+swap+restart on push), 'manual' (persist envelope, operator applies), 'never' (refuse)"`
	AutoRollback           *bool           `json:"auto_rollback,omitempty" jsonschema:"Arm the auto-rollback watchdog's destructive revert: when a self-upgrade's new binary fails to confirm healthy, the supervisor reverts to the previous binary. Default off (observe-only — logs 'would auto-rollback')."`
	Mesh                   *bool           `json:"mesh,omitempty" jsonschema:"Toggle the libp2p mesh data plane (the peer node carrying authenticated, NAT-traversing peer↔peer streams — transport under shard-RPC, peer-backup, the resource fabric). Requires a paired access token."`
	MeshPort               *int            `json:"mesh_port,omitempty" jsonschema:"TCP+QUIC listen port for the mesh host (0 = ephemeral; a stable port helps NAT/hole-punch)"`
	Loom                   *bool           `json:"loom,omitempty" jsonschema:"Toggle running the loom git forge (Gitea) as a managed external binary on a loopback port, auto-exposed over the mesh as the 'git' service. Downloaded/verified/cached by binmgr — not compiled in."`
	LoomPort               *int            `json:"loom_port,omitempty" jsonschema:"loom's loopback HTTP port (0 = default 3000)"`
}

type setBuiltinsOut struct {
	OK             bool `json:"ok"`
	RestartPending bool `json:"restart_pending"`
}

func (s *Server) registerBuiltinsTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_set_builtins",
		Description: "Toggle built-in routes (shell/desktop/clipboard/ssh/sftp/files), the SSH-channel allow flags, the File Browser scope + write switch, and optional local-daemon proxies (podman/ollama, plus the LLM pool / cluster opt-ins). Only fields present in the call are modified.",
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
			Otel:                   in.Otel,
			OtelPool:               in.OtelPool,
			YcodeShare:             in.YcodeShare,
			YcodeShareRequireLogin: in.YcodeShareRequireLogin,
			YcodeShareSurfaces:     in.YcodeShareSurfaces,
			Cluster:                in.Cluster,
			UpdateMode:             in.UpdateMode,
			AutoRollback:           in.AutoRollback,
			Mesh:                   in.Mesh,
			MeshPort:               in.MeshPort,
			Loom:                   in.Loom,
			LoomPort:               in.LoomPort,
		})
		if err != nil {
			return apiErrResult[setBuiltinsOut](err)
		}
		return nil, setBuiltinsOut{OK: res.OK, RestartPending: res.RestartPending}, nil
	})
}
