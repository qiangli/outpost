package admincore

import (
	"context"
	"net"
	"strconv"

	"github.com/qiangli/outpost/internal/agent/runtime"
)

// Node-token retrieval — the HOSTING side of a peer join.
//
// The join credentials split across two owners: the tunnel token and STCP
// secret are minted by outpost itself (control_plane.go), while the k3s node
// token is minted by k3s inside the control-plane container. That last one had
// no surface at all, so the documented join procedure ended with "now exec into
// the container and cat a file".
//
// It is deliberately NOT part of ControlPlaneResult. Reading it costs a
// container exec and can fail for reasons that have nothing to do with the
// config being reported, so folding it into the status view would make a
// healthy config look broken whenever podman was slow or the plane was still
// booting.

// serverNodeToken is a seam for tests — the real implementation shells out to
// podman, which unit tests must not do.
var serverNodeToken = runtime.ServerNodeToken

// NodeTokenResult carries the k3s node token. The value is a CREDENTIAL: it is
// returned only from this explicitly-named operation, never from a status read.
type NodeTokenResult struct {
	OK        bool   `json:"ok"`
	NodeToken string `json:"node_token"`
	// Endpoint is the tunnel address a worker pairs the token with, so the
	// caller can render the whole join line without a second round-trip.
	Endpoint string `json:"endpoint,omitempty"`
}

// ControlPlaneNodeToken reads the k3s node token from the control plane this
// host hosts.
//
// It refuses on a host that is not hosting a plane rather than returning an
// empty value: "this machine has no control plane" and "the control plane has
// no token" need different fixes, and an empty string reads as the second.
func (s *Server) ControlPlaneNodeToken(ctx context.Context) (NodeTokenResult, error) {
	fc, err := s.loadConfig()
	if err != nil {
		return NodeTokenResult{}, err
	}
	if !fc.Cluster.ControlPlaneOn() {
		return NodeTokenResult{}, badRequest(
			"this host does not host a control plane — the node token lives on the hosting machine " +
				"(run `outpost cluster control-plane on` here, or read it there)")
	}
	node := fc.ClusterNodeName()
	if node == "" {
		return NodeTokenResult{}, badRequest("no cluster node name (agent_name unset?)")
	}
	token, err := serverNodeToken(ctx, runtime.ServerOptions{AgentName: node})
	if err != nil {
		// Unavailable, not internal: the container is a dependency that may
		// simply not be up yet, and the caller's remedy is to wait or start it.
		return NodeTokenResult{}, unavailable("%s", err.Error())
	}
	addr, port := fc.Cluster.TunnelBind()
	return NodeTokenResult{
		OK:        true,
		NodeToken: token,
		Endpoint:  net.JoinHostPort(addr, strconv.Itoa(port)),
	}, nil
}
