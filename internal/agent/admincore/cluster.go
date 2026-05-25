package admincore

import (
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/vkpodman"
)

// KubeconfigParams is the wire shape for SetKubeconfig.
//
//   - Kubeconfig: the pasted YAML (k3s.yaml for dev, cloudbox-issued
//     kubeconfig for production).
//   - NodeName: optional override; empty defaults to AgentName at boot.
//   - Enable: when true, also flips Cluster.Enabled so the operator
//     can paste + join in one action.
type KubeconfigParams struct {
	Kubeconfig string `json:"kubeconfig"`
	NodeName   string `json:"node_name,omitempty"`
	Enable     bool   `json:"enable,omitempty"`
}

// KubeconfigResult reports the cluster view after the save plus whether
// the daemon will restart to pick up the join.
type KubeconfigResult struct {
	OK             bool        `json:"ok"`
	Cluster        ClusterView `json:"cluster"`
	RestartPending bool        `json:"restart_pending"`
}

// SetKubeconfig parses a pasted kubeconfig, extracts the apiserver
// URL + bearer token + CA, and persists them into fc.Cluster. The
// kubeconfig itself is NOT stored — only the three fields the runner
// actually uses.
//
// Triggers a restart when fc.Cluster.Enabled ends up true (joining the
// cluster on the next boot) and the host is paired.
func (s *Server) SetKubeconfig(p KubeconfigParams) (KubeconfigResult, error) {
	if p.Kubeconfig == "" {
		return KubeconfigResult{}, badRequest("kubeconfig is required")
	}
	parsed, err := vkpodman.ParseKubeconfig([]byte(p.Kubeconfig))
	if err != nil {
		return KubeconfigResult{}, badRequest("%s", err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return KubeconfigResult{}, err
	}
	if fc.Cluster == nil {
		fc.Cluster = &conf.ClusterConfig{}
	}
	fc.Cluster.APIURL = parsed.APIURL
	fc.Cluster.Token = parsed.Token
	fc.Cluster.CA = parsed.CA
	if p.NodeName != "" {
		fc.Cluster.NodeName = p.NodeName
	}
	if p.Enable {
		fc.Cluster.Enabled = true
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return KubeconfigResult{}, internalErr("%s", err.Error())
	}
	restart := fc.Cluster.Enabled && fc.AgentName != ""
	if restart {
		s.ScheduleRestart()
	}
	return KubeconfigResult{
		OK:             true,
		Cluster:        toClusterView(fc),
		RestartPending: restart,
	}, nil
}

// ClearKubeconfig removes the cluster credentials and the Enabled flag
// so a future boot doesn't try to dial a stale apiserver. Returns
// RestartPending=true when the cluster was previously joined (paired
// hosts) so callers can poll Status.
func (s *Server) ClearKubeconfig() (KubeconfigResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return KubeconfigResult{}, err
	}
	wasEnabled := fc.ClusterOn()
	fc.Cluster = nil
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return KubeconfigResult{}, internalErr("%s", err.Error())
	}
	restart := wasEnabled && fc.AgentName != ""
	if restart {
		s.ScheduleRestart()
	}
	return KubeconfigResult{OK: true, RestartPending: restart}, nil
}
