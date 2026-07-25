package admincore

import (
	"context"
	"log/slog"

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
	// Persist-then-defer (see SetBuiltins comment): tearing down a
	// joined cluster requires a restart, but we let the operator pull
	// the trigger so it can be batched with other settings changes.
	restart := wasEnabled && fc.AgentName != ""
	return KubeconfigResult{OK: true, RestartPending: restart}, nil
}

// LeaveCluster is the per-node "leave DKS" state change — distinct from
// ClearKubeconfig's full wipe. It DISABLES cluster mode but PRESERVES the
// node's identity + MODE (agent stays agent, vk-* stays vk-*), clearing only
// the cloud-ISSUED membership so a rejoin's boot reattach re-fetches fresh
// values (new pod CIDR, overlay key, kubelet port, apiserver creds).
//
// Why preserving Mode is load-bearing: ClusterConfig.Mode == "" normalizes to
// vk-podman (NormalizeClusterMode), so wiping the whole Cluster block — which
// ClearKubeconfig does — makes an agent node silently rejoin as a virtual-
// kubelet node. Leave must keep Mode + NodeName so the node comes back as the
// SAME kind of node. See docs/dks-node-model-and-venues.md.
//
// Disabling (not deleting) the Cluster block means the next boot takes the
// cluster-OFF path, which tears the runtime container down (main.go), instead
// of a stale k3s kubelet retry-looping forever on a Node cloudbox already
// deleted. RestartPending=true when the node was joined so the caller applies it.
func (s *Server) LeaveCluster(ctx context.Context) (KubeconfigResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return KubeconfigResult{}, err
	}
	wasEnabled := fc.ClusterOn()
	if fc.Cluster == nil {
		// Nothing to leave, but make the desired state explicit so a later
		// join has a Mode-bearing block to flip on.
		fc.Cluster = &conf.ClusterConfig{}
	}
	disabled := false
	fc.Cluster.Enabled = &disabled
	// Clear ONLY cloud-issued membership — keep Mode + NodeName (identity).
	fc.Cluster.NodeToken = ""
	fc.Cluster.STCPSecret = ""
	fc.Cluster.APIURL = ""
	fc.Cluster.Token = ""
	fc.Cluster.CA = nil
	fc.Cluster.K8sAPIPort = 0
	fc.Cluster.KubeletProxyPort = 0
	fc.Cluster.OverlayLoginServer = ""
	fc.Cluster.OverlayAuthKey = ""
	fc.Cluster.OverlayPodCIDR = ""
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return KubeconfigResult{}, internalErr("%s", err.Error())
	}
	// Tear the runtime down NOW (not only via the deferred restart) and PURGE
	// the node's identity volumes so a rejoin registers a FRESH overlay
	// identity. cloudbox has already deleted this node's Headscale registration,
	// so re-attaching the persisted machine key would strand the overlay (an IP
	// but no peers, pod-CIDR route never approved). Best-effort: the desired
	// state is already persisted and the boot-time cluster-off teardown is the
	// fallback, so a purge failure must not fail the leave.
	if s.deps.ClusterRuntimeDown != nil {
		if derr := s.deps.ClusterRuntimeDown(ctx, true); derr != nil {
			slog.Warn("LeaveCluster: runtime teardown/purge failed (boot-time fallback applies)", "err", derr)
		}
	}
	return KubeconfigResult{OK: true, RestartPending: wasEnabled && fc.AgentName != ""}, nil
}

// JoinCluster is the symmetric partner to LeaveCluster: it re-ENABLES cluster
// mode, retaining the Mode + NodeName LeaveCluster preserved, so the node
// rejoins as the same kind of node. The cloud-issued credentials LeaveCluster
// cleared are re-fetched by the boot-time reattach — this method only flips the
// desired state on; the reconcile happens on the ensuing restart. Idempotent.
func (s *Server) JoinCluster() (KubeconfigResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return KubeconfigResult{}, err
	}
	if fc.Cluster == nil {
		fc.Cluster = &conf.ClusterConfig{}
	}
	enabled := true
	fc.Cluster.Enabled = &enabled
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return KubeconfigResult{}, internalErr("%s", err.Error())
	}
	return KubeconfigResult{OK: true, RestartPending: fc.AgentName != ""}, nil
}
