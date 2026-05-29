package admincore

import (
	"context"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/userkube"
)

// UserKubeconfigStatus returns the last-known state of the kubectl-
// ready kubeconfig file on disk — path, existence, refresh
// timestamp, last error. Rendered into the admin UI's Cluster
// section so the operator sees at-a-glance whether kubectl is ready
// + what to fix when it isn't.
func (s *Server) UserKubeconfigStatus() userkube.Status {
	return userkube.LastStatus()
}

// RefreshUserKubeconfig re-mints the kubectl-ready kubeconfig from
// cloudbox and rewrites the on-disk file. The admin UI's "Refresh"
// button under the Cluster section drives this; cloudbox-side token
// rotation is the canonical reason to call it. Returns the status
// after the attempt (so the UI can render the new state without a
// second round-trip).
func (s *Server) RefreshUserKubeconfig(ctx context.Context) (userkube.Status, error) {
	fc, err := s.loadConfig()
	if err != nil {
		return userkube.Status{}, err
	}
	if fc.AccessToken == "" {
		return userkube.LastStatus(), badRequest("host not paired (no access_token)")
	}
	node := fc.ClusterNodeName()
	if node == "" {
		node = fc.AgentName
	}
	cloudboxBase := CloudboxHTTPBase(fc)
	_, ferr := userkube.FetchAndWrite(ctx, cloudboxBase, fc.AccessToken, node, "")
	// Always return the updated status (success or failure both
	// captured in userkube.LastStatus). The HTTP layer maps to 200.
	if ferr != nil {
		return userkube.LastStatus(), nil // surface the error via Status.LastError
	}
	return userkube.LastStatus(), nil
}

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
