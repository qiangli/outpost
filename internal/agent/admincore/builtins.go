package admincore

import (
	"github.com/qiangli/outpost/internal/agent/conf"
)

// BuiltinsParams is the partial-update shape for SetBuiltins. Pointer-
// bool fields mean "leave unchanged when nil"; non-nil fields are
// written through to the FileConfig.
//
// The set of fields here is broader than the admin SPA currently
// surfaces — SSHAllowRemoteForward, SSHAllowAgentForward, and
// SSHForwardSockets exist in FileConfig but the SPA has no toggle for
// them. MCP / CLI callers can drive them directly.
type BuiltinsParams struct {
	Shell                 *bool    `json:"shell,omitempty"`
	Desktop               *bool    `json:"desktop,omitempty"`
	Clipboard             *bool    `json:"clipboard,omitempty"`
	SSH                   *bool    `json:"ssh,omitempty"`
	SSHAllowLocalForward  *bool    `json:"ssh_allow_local_forward,omitempty"`
	SSHAllowRemoteForward *bool    `json:"ssh_allow_remote_forward,omitempty"`
	SSHAllowAgentForward  *bool    `json:"ssh_allow_agent_forward,omitempty"`
	SSHForwardSockets     []string `json:"ssh_forward_sockets,omitempty"`
	SFTP                  *bool    `json:"sftp,omitempty"`
	Podman                *bool    `json:"podman,omitempty"`
	Ollama                *bool    `json:"ollama,omitempty"`
	OllamaPool            *bool    `json:"ollama_pool,omitempty"`
	Ycode                 *bool    `json:"ycode,omitempty"`
	Cluster               *bool    `json:"cluster,omitempty"`
	// ClusterMode selects which runtime joins the cluster:
	// "" / "vkpodman" → legacy virtual-kubelet path (default).
	// "agent" → real `k3s agent` subprocess (Linux only). Pointer-string
	// so nil = "leave unchanged"; non-nil with an unknown value is
	// rejected by SetBuiltins with a 400-class APIError.
	ClusterMode           *string  `json:"cluster_mode,omitempty"`
	// UpdateMode is one of "auto" / "manual" / "never" (see
	// conf.UpdateMode* constants). Pointer-string so nil = "leave
	// unchanged"; non-nil with an invalid value is rejected by
	// SetBuiltins with a 400-class APIError.
	UpdateMode            *string  `json:"update_mode,omitempty"`
}

// BuiltinsResult reports what happened. RestartPending is true when
// the change is one the tunnel / built-in routes need to reload to
// observe — callers should poll Status until the daemon is back.
type BuiltinsResult struct {
	OK             bool `json:"ok"`
	RestartPending bool `json:"restart_pending"`
}

// SetBuiltins applies the partial update p to the persisted FileConfig
// and (when the host is paired) schedules a restart so the new toggles
// take effect. On a first-time setup (AgentName empty) nothing is
// mounted yet, so the save is harmless and no restart is triggered.
func (s *Server) SetBuiltins(p BuiltinsParams) (BuiltinsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return BuiltinsResult{}, err
	}
	if p.Shell != nil {
		fc.ShellEnabled = p.Shell
	}
	if p.Desktop != nil {
		fc.DesktopEnabled = p.Desktop
	}
	if p.Clipboard != nil {
		fc.ClipboardEnabled = p.Clipboard
	}
	if p.SSH != nil {
		fc.SSHEnabled = p.SSH
	}
	if p.SSHAllowLocalForward != nil {
		fc.SSHAllowLocalForward = p.SSHAllowLocalForward
	}
	if p.SSHAllowRemoteForward != nil {
		fc.SSHAllowRemoteForward = p.SSHAllowRemoteForward
	}
	if p.SSHAllowAgentForward != nil {
		fc.SSHAllowAgentForward = p.SSHAllowAgentForward
	}
	if p.SSHForwardSockets != nil {
		fc.SSHForwardSockets = p.SSHForwardSockets
	}
	if p.SFTP != nil {
		fc.SFTPEnabled = p.SFTP
	}
	if p.Podman != nil {
		fc.PodmanEnabled = *p.Podman
	}
	if p.Ollama != nil {
		fc.OllamaEnabled = *p.Ollama
	}
	if p.OllamaPool != nil {
		fc.OllamaPoolEnabled = p.OllamaPool
	}
	if p.Ycode != nil {
		fc.YcodeEnabled = *p.Ycode
	}
	if p.Cluster != nil {
		if fc.Cluster == nil {
			fc.Cluster = &conf.ClusterConfig{}
		}
		fc.Cluster.Enabled = *p.Cluster
	}
	if p.ClusterMode != nil {
		mode := *p.ClusterMode
		switch mode {
		case "", "vkpodman", "agent":
			if fc.Cluster == nil {
				fc.Cluster = &conf.ClusterConfig{}
			}
			fc.Cluster.Mode = mode
		default:
			return BuiltinsResult{}, badRequest("cluster_mode must be one of \"\" / vkpodman / agent")
		}
	}
	// UpdateMode is live-read by the upgrade worker on each
	// /admin/upgrade POST, so it doesn't need a restart to take
	// effect. We still save through the same code path because the
	// same FileConfig file owns the value.
	updateModeOnly := p.UpdateMode != nil && p.Shell == nil && p.Desktop == nil && p.Clipboard == nil && p.SSH == nil && p.SSHAllowLocalForward == nil && p.SSHAllowRemoteForward == nil && p.SSHAllowAgentForward == nil && p.SSHForwardSockets == nil && p.SFTP == nil && p.Podman == nil && p.Ollama == nil && p.OllamaPool == nil && p.Ycode == nil && p.Cluster == nil && p.ClusterMode == nil
	if p.UpdateMode != nil {
		if !conf.ValidUpdateMode(*p.UpdateMode) {
			return BuiltinsResult{}, badRequest("update_mode must be one of auto / manual / never")
		}
		fc.UpdateMode = *p.UpdateMode
		// Clear the legacy bool so it doesn't shadow the new field
		// on the next round-trip through UpdateModeName.
		fc.AutoUpgrade = nil
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return BuiltinsResult{}, internalErr("%s", err.Error())
	}
	restart := fc.AgentName != "" && !updateModeOnly
	if restart {
		s.ScheduleRestart()
	}
	return BuiltinsResult{OK: true, RestartPending: restart}, nil
}
