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
	Podman                *bool    `json:"podman,omitempty" jsonschema:"Toggle the built-in podman proxy"`
	Ollama                *bool    `json:"ollama,omitempty" jsonschema:"Toggle the built-in ollama proxy"`
	OllamaPool            *bool    `json:"ollama_pool,omitempty" jsonschema:"Participate in cloudbox's multi-host LLM pool (requires Ollama on)"`
	Cluster               *bool    `json:"cluster,omitempty" jsonschema:"Join the cloudbox virtual-podman cluster as a node"`
}

type setBuiltinsOut struct {
	OK             bool `json:"ok"`
	RestartPending bool `json:"restart_pending"`
}

func (s *Server) registerBuiltinsTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_set_builtins",
		Description: "Toggle built-in routes (shell/desktop/clipboard/ssh/sftp), the SSH-channel allow flags, and optional local-daemon proxies (podman/ollama, plus the LLM pool / cluster opt-ins). Only fields present in the call are modified.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in builtinsIn) (*mcp.CallToolResult, setBuiltinsOut, error) {
		res, err := s.core.SetBuiltins(admincore.BuiltinsParams{
			Shell:                 in.Shell,
			Desktop:               in.Desktop,
			Clipboard:             in.Clipboard,
			SSH:                   in.SSH,
			SSHAllowLocalForward:  in.SSHAllowLocalForward,
			SSHAllowRemoteForward: in.SSHAllowRemoteForward,
			SSHAllowAgentForward:  in.SSHAllowAgentForward,
			SSHForwardSockets:     in.SSHForwardSockets,
			SFTP:                  in.SFTP,
			Podman:                in.Podman,
			Ollama:                in.Ollama,
			OllamaPool:            in.OllamaPool,
			Cluster:               in.Cluster,
		})
		if err != nil {
			return apiErrResult[setBuiltinsOut](err)
		}
		return nil, setBuiltinsOut{OK: res.OK, RestartPending: res.RestartPending}, nil
	})
}
