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
	// Peer is true when LeaveCluster acted on a node that had joined a
	// PEER-hosted control plane rather than the cloudbox-hosted one. Surfaced
	// so a caller (the CLI) knows to SKIP the cloudbox reclaim: cloudbox never
	// issued this node, the peer plane did, and the worker holds no admin
	// credential to delete the Node object from that plane's apiserver — that
	// deletion is the control-plane host's garbage-collection story, not the
	// worker's. omitempty so the cloud path's result is unchanged.
	Peer bool `json:"peer,omitempty"`
}

// ClearKubeconfig disables DKS and removes cloud-issued membership while
// preserving the configured runtime set. Callers apply teardown through the
// pending restart.
func (s *Server) ClearKubeconfig() (KubeconfigResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return KubeconfigResult{}, err
	}
	wasEnabled := fc.ClusterOn()
	disableClusterMembership(fc)
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return KubeconfigResult{}, internalErr("%s", err.Error())
	}
	// Persist-then-defer (see SetBuiltins comment): tearing down a
	// joined cluster requires a restart, but we let the operator pull
	// the trigger so it can be batched with other settings changes.
	restart := wasEnabled && fc.AgentName != ""
	return KubeconfigResult{OK: true, RestartPending: restart}, nil
}

func disableClusterMembership(fc *conf.FileConfig) {
	if fc.Cluster == nil {
		fc.Cluster = &conf.ClusterConfig{}
	}
	disabled := false
	fc.Cluster.Enabled = &disabled

	// Cloud-issued membership. A rejoin's boot reattach re-fetches all of it,
	// so clearing it is what lets a rejoin start from a clean slate rather than
	// presenting stale credentials to an apiserver that has moved on.
	fc.Cluster.NodeToken = ""
	fc.Cluster.APIURL = ""
	fc.Cluster.Token = ""
	fc.Cluster.CA = nil
	fc.Cluster.K8sAPIPort = 0
	fc.Cluster.KubeletProxyPort = 0
	fc.Cluster.OverlayLoginServer = ""
	fc.Cluster.OverlayAuthKey = ""
	fc.Cluster.OverlayPodCIDR = ""

	// Peer-plane membership (the worker-side twin of the hosting fields). A
	// peer-joined node keeps join_endpoint/join_token pointing at its plane;
	// leaving must drop them too, or the node stays half-attached — JoinsPeerPlane
	// still true, the next boot's frpc still dialing a plane the operator left.
	// A rejoin re-supplies these from the hosting machine.
	fc.Cluster.JoinEndpoint = ""
	fc.Cluster.JoinToken = ""

	// STCPSecret is dual-purpose: a WORKER's visitor secret AND a hosting
	// control plane's PUBLISHED secret. Clearing it is correct for a worker but
	// would corrupt a plane this host HOSTS — every worker joined to it would
	// fail to reach the apiserver. So preserve it (and, implicitly, the rest of
	// the hosting block — control_plane / tunnel_token / tunnel bind /
	// control_plane_api_addr / control_plane_kubeconfig are never touched here)
	// when this host is itself a control plane.
	if !fc.Cluster.ControlPlaneOn() {
		fc.Cluster.STCPSecret = ""
	}
}

// LeaveCluster is the per-node "leave DKS" state change — distinct from
// ClearKubeconfig's full wipe. It DISABLES cluster mode but PRESERVES the
// node identities + runtime set, clearing only the MEMBERSHIP fields so a
// rejoin's boot reattach (cloud plane) or a fresh `outpost cluster join`
// (peer plane) re-supplies them.
//
// It is membership-only by construction: it never touches cloudbox pairing
// (access_token) or any app / shell / LLM / outbound / mesh setting, so
// leaving the cluster does not log the host out of the portal or drop unrelated
// services. It works for BOTH a cloud-managed node and a peer-joined worker —
// disableClusterMembership clears the cloud-issued creds AND the peer
// join_endpoint/join_token, while preserving the hosting block when this host
// is itself a control plane (see disableClusterMembership).
//
// Disabling (not deleting) the Cluster block means the next boot takes the
// cluster-OFF path, which tears the runtime container down (main.go), instead
// of a stale k3s kubelet retry-looping forever on a Node the plane already
// deleted. RestartPending=true when the node was joined so the caller applies
// it. Idempotent: a second call on an already-left node is a no-op save with
// RestartPending=false.
//
// Result.Peer reports whether this was a peer-joined worker. The CLI uses it to
// skip the cloudbox reclaim — cloudbox never issued this node. Deleting the
// Node object from the PEER apiserver is deliberately NOT done here: the worker
// holds only a k3s join token, not an admin credential for that plane, and
// leave does not add one. That deletion is the control-plane host's
// garbage-collection story.
func (s *Server) LeaveCluster(ctx context.Context) (KubeconfigResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return KubeconfigResult{}, err
	}
	wasEnabled := fc.ClusterOn()
	wasPeer := fc.Cluster.JoinsPeerPlane()
	disableClusterMembership(fc)
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
	return KubeconfigResult{
		OK:             true,
		Peer:           wasPeer,
		RestartPending: wasEnabled && fc.AgentName != "",
	}, nil
}

// JoinCluster is the symmetric partner to LeaveCluster: it re-ENABLES cluster
// mode, retaining the runtimes + NodeName LeaveCluster preserved. The cloud-issued credentials LeaveCluster
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
	if err := fc.Cluster.ValidateRuntimes(); err != nil {
		return KubeconfigResult{}, badRequest("%s", err.Error())
	}
	enabled := true
	fc.Cluster.Enabled = &enabled
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return KubeconfigResult{}, internalErr("%s", err.Error())
	}
	return KubeconfigResult{OK: true, RestartPending: fc.AgentName != ""}, nil
}
