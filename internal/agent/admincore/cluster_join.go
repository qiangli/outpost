package admincore

import (
	"net"
	"strconv"
	"strings"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// Peer-plane JOIN surface — the worker-side twin of control_plane.go.
//
// Hosting a plane got four surfaces when placement became a choice; joining
// one did not, so `cluster.join_endpoint` / `join_token` were reachable only
// by hand-editing agent.json. That made the peer-cluster half of the feature
// built-and-unreachable in exactly the way the hosting half used to be.
//
// WHAT A WORKER ACTUALLY NEEDS, and why this takes more than an endpoint:
// the hosting side hands out FOUR values, and a worker given a subset fails in
// a way that reads like a network fault rather than a missing credential.
//
//   - join_endpoint — where the host's frps listens (host[:port], default 7000)
//   - join_token    — authenticates the frpc SESSION (cluster.tunnel_token there)
//   - stcp_secret   — authorizes the visitor that carries the apiserver
//   - node_token    — the k3s join token (`outpost cluster token` there)
//
// The last three are CREDENTIALS. None of them is ever returned by a status
// read: presence is reported as has_*, matching the treatment Token and
// MCPBearerToken already get.

// PeerPlaneParams is a partial update. A nil field means "leave alone", so
// re-running the join with a rotated token does not clear the other three.
type PeerPlaneParams struct {
	// Endpoint is the peer's tunnel server as host or host:port. A bare host
	// takes conf.DefaultTunnelBindPort.
	Endpoint *string `json:"endpoint,omitempty"`
	// Token is the peer's cluster.tunnel_token.
	Token *string `json:"token,omitempty"`
	// STCPSecret authorizes this worker's visitor of the published apiserver.
	STCPSecret *string `json:"stcp_secret,omitempty"`
	// NodeToken is the k3s join token the hosting side prints with
	// `outpost cluster token`.
	NodeToken *string `json:"node_token,omitempty"`
	// APIPort is the LOCAL port this worker's visitor binds the joined
	// apiserver on (cluster.k8s_api_port). Optional; 0 means the 6443 default.
	APIPort *int `json:"api_port,omitempty"`

	// Agent / Virtual select which Nodes this worker registers on the joined
	// plane — cluster.runtimes.agent and cluster.runtimes.virtual, the same
	// two fields SetBuiltins writes and with the same partial-update rules
	// (nil leaves the persisted value alone; a non-nil Virtual REPLACES the
	// complete set, so a non-nil empty slice deselects every virtual backend).
	//
	// A peer plane can host virtual-kubelet nodes as-is: vknode's kubeconfig
	// loader already accepts the client-certificate credentials k3s issues, as
	// distinct from a cloudbox-minted bearer token (see vknode/kubeconfig.go).
	// So this is selection, not new runtime support — before it existed a
	// worker could only reach a vk node by hand-editing agent.json.
	//
	// Leaving BOTH nil preserves the historical behaviour exactly: the join
	// falls through to selecting the agent runtime, and only when nothing is
	// selected already.
	Agent   *bool    `json:"cluster_agent,omitempty"`
	Virtual []string `json:"cluster_virtual,omitempty"`
}

// PeerPlaneResult is the redacted join status. It never carries a credential —
// the three secrets are reported as presence flags only.
type PeerPlaneResult struct {
	OK bool `json:"ok"`
	// Joined is true when a peer endpoint is configured, i.e. this host joins
	// a plane OTHER than the cloudbox-hosted one.
	Joined   bool   `json:"joined"`
	Endpoint string `json:"endpoint,omitempty"`
	APIPort  int    `json:"api_port,omitempty"`

	HasToken      bool `json:"has_token"`
	HasSTCPSecret bool `json:"has_stcp_secret"`
	HasNodeToken  bool `json:"has_node_token"`

	// ClusterEnabled reflects cluster.enabled AFTER the operation. Surfaced
	// because a join that persisted its credentials but left cluster mode off
	// looks successful and does nothing.
	ClusterEnabled bool `json:"cluster_enabled"`
	RestartPending bool `json:"restart_pending"`

	// RuntimeAgent / RuntimeVirtual report the runtime selection AFTER the
	// operation — which Nodes this worker will register. Reported because the
	// join's default is implicit (agent when nothing was selected), so without
	// this an operator cannot tell a defaulted selection from one they made.
	RuntimeAgent   bool     `json:"runtime_agent"`
	RuntimeVirtual []string `json:"runtime_virtual,omitempty"`
}

// PeerPlaneView reports which control plane this host joins. Redacted — there
// is no reveal counterpart, because unlike the hosting side's token these are
// values the operator brought WITH them; the source of truth is the host that
// issued them.
func (s *Server) PeerPlaneView() (PeerPlaneResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return PeerPlaneResult{}, err
	}
	return peerPlaneResult(fc), nil
}

// JoinPeerPlane points this host at a peer-hosted control plane.
//
// It ENABLES cluster mode (selecting the agent runtime when no runtime is
// configured) as part of the same save. A "join" that persisted an endpoint
// and left the node not joining anything would be a config editor wearing a
// verb's name.
//
// Cluster runtime config is read once at boot, so any change is restart-
// pending and ScheduleRestart is called — same contract as SetControlPlane.
func (s *Server) JoinPeerPlane(p PeerPlaneParams) (PeerPlaneResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return PeerPlaneResult{}, err
	}
	if fc.Cluster == nil {
		fc.Cluster = &conf.ClusterConfig{}
	}

	if p.Endpoint != nil {
		ep, err := normalizeJoinEndpoint(*p.Endpoint)
		if err != nil {
			return PeerPlaneResult{}, err
		}
		fc.Cluster.JoinEndpoint = ep
	}
	if p.Token != nil {
		fc.Cluster.JoinToken = strings.TrimSpace(*p.Token)
	}
	if p.STCPSecret != nil {
		fc.Cluster.STCPSecret = strings.TrimSpace(*p.STCPSecret)
	}
	if p.NodeToken != nil {
		fc.Cluster.NodeToken = strings.TrimSpace(*p.NodeToken)
	}
	if p.APIPort != nil {
		port := *p.APIPort
		if port < 0 || port > 65535 {
			return PeerPlaneResult{}, badRequest("api_port must be 0-65535, got %d", port)
		}
		fc.Cluster.K8sAPIPort = port
	}

	// Validate the RESULT, not the input: a second call that supplies only a
	// rotated token must still land on a complete configuration, and a first
	// call that supplies only a token must not silently produce a host that
	// joins nothing.
	if strings.TrimSpace(fc.Cluster.JoinEndpoint) == "" {
		return PeerPlaneResult{}, badRequest("join endpoint required (the peer's tunnel server, host[:port])")
	}
	if strings.TrimSpace(fc.Cluster.JoinToken) == "" {
		return PeerPlaneResult{}, badRequest(
			"join token required — read it on the hosting machine with `outpost cluster control-plane token`")
	}

	// An EXPLICIT runtime selection is applied first, with the same
	// partial-update rules SetBuiltins uses. This is the only way a join can
	// change an existing selection — and it is not a clobber, because the
	// operator named the field.
	if p.Agent != nil {
		fc.Cluster.Runtimes.Agent = *p.Agent
	}
	if p.Virtual != nil {
		virtual, err := conf.NormalizeVirtualRuntimes(p.Virtual)
		if err != nil {
			return PeerPlaneResult{}, badRequest("cluster_virtual: %s", err.Error())
		}
		fc.Cluster.Runtimes.Virtual = virtual
	}

	// Selecting a runtime is what turns membership into a node. Only fill it
	// in when the operator has not chosen: overwriting a virtual-only
	// selection would silently start a k3s agent they did not ask for. This
	// runs only for a join that named no runtime at all — the historical
	// default, unchanged. An operator who deselected everything explicitly
	// falls through to ValidateRuntimes and gets told so, rather than having
	// an agent runtime quietly reinstated under the selection they just made.
	explicitRuntimes := p.Agent != nil || p.Virtual != nil
	if !explicitRuntimes && !fc.Cluster.Runtimes.Agent && len(fc.Cluster.Runtimes.Virtual) == 0 {
		fc.Cluster.Runtimes.Agent = true
	}
	if err := fc.Cluster.ValidateRuntimes(); err != nil {
		return PeerPlaneResult{}, badRequest("%s", err.Error())
	}
	enabled := true
	fc.Cluster.Enabled = &enabled

	// Joining a peer plane makes the CLOUD plane's overlay credentials stale,
	// and the runtime joins an overlay purely on overlay_login_server being
	// non-empty — so leaving the trio behind attaches this node to the cloud
	// overlay while its k3s agent joins the peer plane (B6). The boot reattach
	// also clears them, but that needs cloudbox reachable; clearing at the
	// switch itself covers a host that next boots offline. A peer plane never
	// issues these three, so this can only ever drop cloudbox leftovers.
	fc.Cluster.OverlayLoginServer = ""
	fc.Cluster.OverlayAuthKey = ""
	fc.Cluster.OverlayPodCIDR = ""

	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return PeerPlaneResult{}, internalErr("%s", err.Error())
	}

	out := peerPlaneResult(fc)
	out.RestartPending = fc.AgentName != ""
	if out.RestartPending {
		s.ScheduleRestart()
	}
	return out, nil
}

// LeavePeerPlane reverts this host to the cloudbox-hosted plane.
//
// It clears the endpoint, the join token, AND the two peer-issued credentials
// (node token, STCP secret) — those describe the PEER's cluster, and leaving
// them behind is what makes the next boot's reattach look broken: cloudbox
// refuses to refresh cluster membership while a peer endpoint is configured
// (see applyCloudboxClusterMembership), so a half-cleared config would keep
// presenting a foreign node token to cloudbox's apiserver and fail the join
// with a CA-hash mismatch.
//
// Cluster mode itself is LEFT ALONE. "Stop joining that plane" is not "stop
// being a cluster node" — the cloudbox-hosted plane is the default, and the
// boot reattach re-fetches its credentials. Use `outpost cluster leave` to
// leave the cluster entirely.
func (s *Server) LeavePeerPlane() (PeerPlaneResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return PeerPlaneResult{}, err
	}
	if fc.Cluster == nil {
		fc.Cluster = &conf.ClusterConfig{}
	}
	was := fc.Cluster.JoinsPeerPlane()
	fc.Cluster.JoinEndpoint = ""
	fc.Cluster.JoinToken = ""
	fc.Cluster.NodeToken = ""
	fc.Cluster.STCPSecret = ""

	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return PeerPlaneResult{}, internalErr("%s", err.Error())
	}
	out := peerPlaneResult(fc)
	out.RestartPending = was && fc.AgentName != ""
	if out.RestartPending {
		s.ScheduleRestart()
	}
	return out, nil
}

// normalizeJoinEndpoint validates an operator-supplied endpoint and returns it
// in host:port form.
//
// A URL is rejected rather than coerced: frps speaks its own protocol on a raw
// TCP port, so `https://peer:7000` is not a slightly-wrong spelling of the
// right thing — it is a sign the operator copied the apiserver's address, and
// silently stripping the scheme would hide that.
func normalizeJoinEndpoint(raw string) (string, error) {
	ep := strings.TrimSpace(raw)
	if ep == "" {
		return "", badRequest("join endpoint required (the peer's tunnel server, host[:port])")
	}
	if strings.Contains(ep, "://") {
		return "", badRequest(
			"join endpoint is a host[:port], not a URL — got %q (the tunnel server is a raw TCP port, typically %d)",
			ep, conf.DefaultTunnelBindPort)
	}
	if strings.ContainsAny(ep, "/ \t") {
		return "", badRequest("join endpoint must be host[:port], got %q", ep)
	}

	host, portStr, err := net.SplitHostPort(ep)
	if err != nil {
		// A bare host (or a bare IPv6 literal) — take the default port, which
		// is what the hosting side binds unless told otherwise.
		if ip := net.ParseIP(ep); ip != nil || !strings.Contains(ep, ":") {
			return net.JoinHostPort(ep, strconv.Itoa(conf.DefaultTunnelBindPort)), nil
		}
		return "", badRequest("join endpoint must be host[:port], got %q", ep)
	}
	if strings.TrimSpace(host) == "" {
		return "", badRequest("join endpoint needs a host, got %q", ep)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", badRequest("join endpoint port must be 1-65535, got %q", portStr)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func peerPlaneResult(fc *conf.FileConfig) PeerPlaneResult {
	out := PeerPlaneResult{OK: true}
	if fc == nil || fc.Cluster == nil {
		return out
	}
	cc := fc.Cluster
	out.Joined = cc.JoinsPeerPlane()
	out.Endpoint = cc.JoinEndpoint
	out.APIPort = cc.K8sAPIPort
	out.HasToken = cc.JoinToken != ""
	out.HasSTCPSecret = cc.STCPSecret != ""
	out.HasNodeToken = cc.NodeToken != ""
	out.ClusterEnabled = fc.ClusterOn()
	out.RuntimeAgent = cc.HasAgentRuntime()
	out.RuntimeVirtual = cc.VirtualRuntimes()
	return out
}
