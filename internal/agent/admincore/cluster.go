package admincore

import (
	"github.com/qiangli/outpost/internal/agent/conf"
)

// KubeconfigResult reports the cluster view after a mutation plus
// whether the daemon will restart to apply it. Returned from
// ClearKubeconfig today; previously also from SetKubeconfig (the
// bring-your-own paste path, removed — outposts only join their
// owning cloudbox's cluster now; for a different cluster, pair a
// second outpost against that cloudbox).
type KubeconfigResult struct {
	OK             bool        `json:"ok"`
	Cluster        ClusterView `json:"cluster"`
	RestartPending bool        `json:"restart_pending"`
}

// ClearKubeconfig removes the cluster credentials and the Enabled
// flag so a future boot doesn't try to dial a stale apiserver. Used
// by the "Leave cluster" affordance in the admin UI. Returns
// RestartPending=true when the cluster was previously joined so the
// caller can poll Status.
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
