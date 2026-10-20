package admincore

import (
	"context"
	"log/slog"
	"strings"

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
	ClearCachedClusterJoin(fc.Cluster)
}

// ClearCachedClusterJoin drops every cached cluster-membership field from cc
// — the field list `outpost cluster clear` wipes, minus the enabled bit and
// the runtime selection, which are the operator's. It reports whether
// anything was cleared. This is the ONE place that list lives: the clear
// path (disableClusterMembership) and the re-pair path
// (DropClusterJoinOnServerChange) both call it, so a membership field added
// later is forgotten by neither.
func ClearCachedClusterJoin(cc *conf.ClusterConfig) bool {
	if cc == nil {
		return false
	}
	before := *cc

	// Cloud-issued membership. A rejoin's boot reattach re-fetches all of it,
	// so clearing it is what lets a rejoin start from a clean slate rather than
	// presenting stale credentials to an apiserver that has moved on.
	cc.NodeToken = ""
	cc.APIURL = ""
	cc.Token = ""
	cc.CA = nil
	cc.K8sAPIPort = 0
	cc.KubeletProxyPort = 0
	cc.OverlayLoginServer = ""
	cc.OverlayAuthKey = ""
	cc.OverlayPodCIDR = ""

	// Peer-plane membership (the worker-side twin of the hosting fields). A
	// peer-joined node keeps join_endpoint/join_token pointing at its plane;
	// leaving must drop them too, or the node stays half-attached — JoinsPeerPlane
	// still true, the next boot's frpc still dialing a plane the operator left.
	// A rejoin re-supplies these from the hosting machine.
	cc.JoinEndpoint = ""
	cc.JoinToken = ""

	// STCPSecret is dual-purpose: a WORKER's visitor secret AND a hosting
	// control plane's PUBLISHED secret. Clearing it is correct for a worker but
	// would corrupt a plane this host HOSTS — every worker joined to it would
	// fail to reach the apiserver. So preserve it (and, implicitly, the rest of
	// the hosting block — control_plane / tunnel_token / tunnel bind /
	// control_plane_api_addr / control_plane_kubeconfig are never touched here)
	// when this host is itself a control plane.
	if !cc.ControlPlaneOn() {
		cc.STCPSecret = ""
	}

	return before.NodeToken != cc.NodeToken ||
		before.APIURL != cc.APIURL ||
		before.Token != cc.Token ||
		len(before.CA) != 0 ||
		before.K8sAPIPort != cc.K8sAPIPort ||
		before.KubeletProxyPort != cc.KubeletProxyPort ||
		before.OverlayLoginServer != cc.OverlayLoginServer ||
		before.OverlayAuthKey != cc.OverlayAuthKey ||
		before.OverlayPodCIDR != cc.OverlayPodCIDR ||
		before.JoinEndpoint != cc.JoinEndpoint ||
		before.JoinToken != cc.JoinToken ||
		before.STCPSecret != cc.STCPSecret
}

// PairingServerChanged reports whether a re-pair moves this host from the
// server whose HTTP base is prev to one whose base is next (both as
// CloudboxHTTPBase renders them). The cluster join endpoint is built from
// that base, so comparing bases is exactly "would the cluster endpoint
// change": a different host is a change; the same host on a different
// port or scheme counts only when the rendered base differs (wss:443 and
// https:443 render identically; wss:443 and wss:8443 do not). An empty
// prev — a host that was never paired, or was unpaired first — is NOT a
// change: there is no previous server to have issued the cached join, so
// nothing is dropped (a dev config pointing cluster.api_url at a local
// k3s stays as the operator wrote it).
func PairingServerChanged(prev, next string) bool {
	prev = strings.ToLower(strings.TrimSpace(prev))
	next = strings.ToLower(strings.TrimSpace(next))
	return prev != "" && prev != next
}

// DropClusterJoinOnServerChange is the re-pair half of S482: when the
// pairing server changes, a PLAIN server-plane member drops its cached join
// (the same fields `outpost cluster clear` wipes) so the next boot fetches a
// fresh one from the new server instead of dialing the previous server's
// apiserver forever. The operator's opt-in (cluster.enabled) and runtime
// selection are kept — the host asked to be a cluster node, and the pairing
// is what moved.
//
// It leaves two kinds of host alone, whatever the server did:
//   - a peer-plane member (JoinsPeerPlane): its credentials belong to the
//     peer's plane, which did not move;
//   - a control-plane host (ControlPlaneOn): it mints its own.
//
// A same-server re-pair (a replacement instance at the same address) keeps
// the join. Reports whether anything was dropped and logs one line (no
// secrets) when it was. Callers hold whatever lock guards fc.
func DropClusterJoinOnServerChange(fc *conf.FileConfig, prevBase, nextBase string) bool {
	if fc == nil || fc.Cluster == nil || !PairingServerChanged(prevBase, nextBase) {
		return false
	}
	cc := fc.Cluster
	if cc.JoinsPeerPlane() || cc.ControlPlaneOn() {
		return false
	}
	if !ClearCachedClusterJoin(cc) {
		return false
	}
	slog.Info("pairing moved to a different server; dropped the cached cluster join (re-issued on next boot)",
		"previous", prevBase, "next", nextBase)
	return true
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
//
// The runtime teardown itself is also plane-aware (see the purge comment
// inline below): a cloud-managed leave purges the local overlay identity
// because cloudbox has already deregistered it from Headscale, while a
// peer-joined leave PRESERVES the overlay identity — no deregistration ever
// happened (leave makes no call to the peer plane), so purging here would
// desync the local machine key from a registration that still exists
// wherever the peer plane's overlay is registered.
func (s *Server) LeaveCluster(ctx context.Context) (KubeconfigResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return KubeconfigResult{}, err
	}
	// Determine BEFORE clearing: disableClusterMembership wipes the fields
	// both of these read.
	wasActive := fc.ClusterOn()
	wasPeer := fc.Cluster.JoinsPeerPlane()
	disableClusterMembership(fc)
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return KubeconfigResult{}, internalErr("%s", err.Error())
	}
	// Tear the runtime down NOW (not only via the deferred restart), but only
	// when the node was actually active — an already-left node (second `leave`
	// call, or one that was never joined) has no runtime to stop and no
	// identity to purge, so skip the call rather than re-invoking it as a
	// no-op every time.
	//
	// purge differs by plane:
	//   - cloud-managed (wasPeer=false): purge=true. cloudbox has already
	//     deleted this node's Headscale registration (the cloudbox reclaim
	//     call below/at the CLI layer), so re-attaching the persisted machine
	//     key would strand the overlay (an IP but no peers, pod-CIDR route
	//     never approved) — a fresh identity on rejoin is required.
	//   - peer-joined (wasPeer=true): purge=false. Leave never contacts the
	//     peer plane or any headscale/overlay registrar — the worker holds no
	//     admin credential to deregister anything remotely. Purging the local
	//     machine key here would orphan a registration that still exists on
	//     the peer side, so the overlay identity is deliberately PRESERVED;
	//     a later rejoin reuses it rather than presenting a peer/registrar
	//     mismatch.
	// Best-effort either way: the desired state is already persisted and the
	// boot-time cluster-off teardown is the fallback, so a failure here must
	// not fail the leave.
	if wasActive && s.deps.ClusterRuntimeDown != nil {
		purge := !wasPeer
		if derr := s.deps.ClusterRuntimeDown(ctx, purge); derr != nil {
			slog.Warn("LeaveCluster: runtime teardown/purge failed (boot-time fallback applies)", "err", derr, "purge", purge)
		}
	}
	return KubeconfigResult{
		OK:             true,
		Peer:           wasPeer,
		RestartPending: wasActive && fc.AgentName != "",
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
