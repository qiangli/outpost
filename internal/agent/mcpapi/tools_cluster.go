package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

type clearClusterOut struct {
	OK             bool `json:"ok"`
	RestartPending bool `json:"restart_pending"`
}

// registerClusterTools — outpost_set_kubeconfig is gone; the
// bring-your-own paste path is no longer supported (outposts only
// join their owning cloudbox's cluster; for a different cluster,
// pair another outpost). outpost_clear_kubeconfig stays because
// "leave the cluster" remains a useful operator action.
func (s *Server) registerClusterTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_clear_kubeconfig",
		Description: "Clear the cluster credentials and the Enabled flag (i.e. leave the cluster). Joining happens via `outpost_set_builtins {cluster: true}` once paired; the daemon auto-fetches a kubeconfig from cloudbox on next boot.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, clearClusterOut, error) {
		res, err := s.core.ClearKubeconfig()
		if err != nil {
			return apiErrResult[clearClusterOut](err)
		}
		return nil, clearClusterOut{OK: res.OK, RestartPending: res.RestartPending}, nil
	})

	// outpost_cluster_leave / _join are the node-lifecycle pair, distinct from
	// clear_kubeconfig (a full wipe). leave DISABLES cluster mode but PRESERVES
	// this node's identity + mode so a rejoin comes back as the SAME kind of
	// node (agent stays agent), clearing only MEMBERSHIP — the cloud-issued
	// credentials AND, for a peer-joined worker, the peer join_endpoint/
	// join_token — while preserving the hosting block when this host is itself a
	// control plane; join re-enables it. cloudbox pairing and unrelated settings
	// are untouched. The runtime container is reconciled on the ensuing restart:
	// leave's cluster-off boot tears it down, join's cluster-on boot recreates it
	// fresh (re-registering the kubelet). See docs/dks-node-model-and-venues.md.
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_cluster_leave",
		Description: "Leave the DKS cluster as this node: disable cluster mode, PRESERVING the node's mode + identity (agent stays agent), and clear only membership (cloud-issued creds, plus a peer worker's join endpoint/token) so a rejoin starts clean. A peer worker's Node object is deleted by the control-plane host's GC, not here. Pair with a restart to tear the runtime down.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, clearClusterOut, error) {
		res, err := s.core.LeaveCluster(ctx)
		if err != nil {
			return apiErrResult[clearClusterOut](err)
		}
		return nil, clearClusterOut{OK: res.OK, RestartPending: res.RestartPending}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_cluster_join",
		Description: "Rejoin the DKS cluster as this node: re-enable cluster mode, retaining the preserved mode + identity. The boot reattach re-fetches fresh credentials; pair with a restart to bring the runtime up fresh.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, clearClusterOut, error) {
		res, err := s.core.JoinCluster()
		if err != nil {
			return apiErrResult[clearClusterOut](err)
		}
		return nil, clearClusterOut{OK: res.OK, RestartPending: res.RestartPending}, nil
	})

	s.registerControlPlaneTools()
	s.registerPeerPlaneTools()
}

// controlPlaneIn is a partial update: an omitted field leaves the current
// value alone, so toggling the switch never silently resets a custom bind.
type controlPlaneIn struct {
	Enabled  *bool   `json:"enabled,omitempty" jsonschema:"host the apiserver on this machine"`
	BindAddr *string `json:"bind_addr,omitempty" jsonschema:"IP the tunnel server binds; defaults to 127.0.0.1"`
	BindPort *int    `json:"bind_port,omitempty" jsonschema:"port the tunnel server binds; defaults to 7000"`
}

type controlPlaneOut struct {
	OK               bool   `json:"ok"`
	ControlPlane     bool   `json:"control_plane"`
	BindAddr         string `json:"bind_addr"`
	BindPort         int    `json:"bind_port"`
	HasToken         bool   `json:"has_token"`
	TunnelToken      string `json:"tunnel_token,omitempty"`
	STCPSecret       string `json:"stcp_secret,omitempty"`
	APIAddr          string `json:"api_addr,omitempty"`
	LANExposed       bool   `json:"lan_exposed"`
	RestartPending   bool   `json:"restart_pending"`
	WorkerRejoinHint string `json:"worker_rejoin_hint,omitempty"`
}

func toControlPlaneOut(res admincore.ControlPlaneResult) controlPlaneOut {
	return controlPlaneOut{
		OK:               res.OK,
		ControlPlane:     res.ControlPlane,
		BindAddr:         res.BindAddr,
		BindPort:         res.BindPort,
		HasToken:         res.HasToken,
		TunnelToken:      res.TunnelToken,
		STCPSecret:       res.STCPSecret,
		APIAddr:          res.APIAddr,
		LANExposed:       res.LANExposed,
		RestartPending:   res.RestartPending,
		WorkerRejoinHint: res.WorkerRejoinHint,
	}
}

// registerControlPlaneTools exposes control-plane PLACEMENT — whether this
// host runs the apiserver other machines join, rather than merely joining one.
//
// Reveal is its own tool rather than a flag on the status tool so an agent
// reading cluster state never incidentally pulls a joining credential into a
// transcript.
func (s *Server) registerControlPlaneTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_control_plane",
		Description: "Report whether this host hosts the DKS apiserver, and where its tunnel server binds. Does NOT return the join token — use outpost_control_plane_token for that.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, controlPlaneOut, error) {
		res, err := s.core.ControlPlaneView(false)
		if err != nil {
			return apiErrResult[controlPlaneOut](err)
		}
		return nil, toControlPlaneOut(res), nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_set_control_plane",
		Description: "Host the DKS apiserver on this machine (or stop hosting it), and set where its tunnel server binds. Enabling mints the join token if absent. Bind defaults to 127.0.0.1 — set bind_addr to 0.0.0.0 only to accept joins from the network. Triggers a restart.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in controlPlaneIn) (*mcp.CallToolResult, controlPlaneOut, error) {
		res, err := s.core.SetControlPlane(admincore.ControlPlaneParams{
			Enabled: in.Enabled, BindAddr: in.BindAddr, BindPort: in.BindPort,
		})
		if err != nil {
			return apiErrResult[controlPlaneOut](err)
		}
		return nil, toControlPlaneOut(res), nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_control_plane_token",
		Description: "Reveal the tunnel token workers need to join the control plane hosted here. This is a credential — treat it like the k3s node-token.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, controlPlaneOut, error) {
		res, err := s.core.ControlPlaneView(true)
		if err != nil {
			return apiErrResult[controlPlaneOut](err)
		}
		return nil, toControlPlaneOut(res), nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_rotate_control_plane_token",
		Description: "Mint a new tunnel token, returning it. EVERY WORKER MUST BE RECONFIGURED — existing workers fail their next reconnect. The response's worker_rejoin_hint carries the exact recovery command (only the token changed; stcp_secret and node_token stay valid). Use to revoke a leaked token, not as routine hygiene.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, controlPlaneOut, error) {
		res, err := s.core.RotateControlPlaneToken()
		if err != nil {
			return apiErrResult[controlPlaneOut](err)
		}
		return nil, toControlPlaneOut(res), nil
	})

	s.registerControlPlaneStatusTool()
}

type controlPlaneStatusOut struct {
	Hosted              bool               `json:"hosted"`
	ContainerExists     bool               `json:"container_exists"`
	ContainerRunning    bool               `json:"container_running"`
	APIServerServing    bool               `json:"apiserver_serving"`
	APIServerStatusCode int                `json:"apiserver_status_code,omitempty"`
	Nodes               []controlPlaneNode `json:"nodes"`
	NodeCount           int                `json:"node_count"`
	JoinEndpoint        string             `json:"join_endpoint,omitempty"`
	HasJoinToken        bool               `json:"has_join_token"`
	HasNodeToken        bool               `json:"has_node_token"`
	HasSTCPSecret       bool               `json:"has_stcp_secret"`
	CheckedAt           int64              `json:"checked_at,omitzero"`
}

type controlPlaneNode struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
}

func toControlPlaneStatusOut(s admincore.ControlPlaneStatus) controlPlaneStatusOut {
	nodes := make([]controlPlaneNode, len(s.Nodes))
	for i, n := range s.Nodes {
		nodes[i] = controlPlaneNode{Name: n.Name, Ready: n.Ready}
	}
	checkedAt := s.CheckedAt.Unix()
	if s.CheckedAt.IsZero() {
		checkedAt = 0
	}
	return controlPlaneStatusOut{
		Hosted:              s.Hosted,
		ContainerExists:     s.ContainerExists,
		ContainerRunning:    s.ContainerRunning,
		APIServerServing:    s.APIServerServing,
		APIServerStatusCode: s.APIServerStatusCode,
		Nodes:               nodes,
		NodeCount:           s.NodeCount,
		JoinEndpoint:        s.JoinEndpoint,
		HasJoinToken:        s.HasJoinToken,
		HasNodeToken:        s.HasNodeToken,
		HasSTCPSecret:       s.HasSTCPSecret,
		CheckedAt:           checkedAt,
	}
}

func (s *Server) registerControlPlaneStatusTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_control_plane_status",
		Description: "Report the health and readiness of the hosted control plane: container state, apiserver serving, cluster node list with readiness. Credential presence reported as booleans only.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, controlPlaneStatusOut, error) {
		status, err := s.core.ControlPlaneStatusView(ctx)
		if err != nil {
			return apiErrResult[controlPlaneStatusOut](err)
		}
		return nil, toControlPlaneStatusOut(status), nil
	})
}
