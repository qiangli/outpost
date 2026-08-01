package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// Peer-plane join tools.
//
// NAMING: these are `outpost_cluster_join_peer` / `_leave_peer`, not
// `outpost_cluster_join` / `_leave` — those names are already taken by the
// node-lifecycle pair (re-enable / disable cluster mode against whichever plane
// is configured). The distinction is real: this pair changes WHICH plane, that
// pair changes WHETHER this host is a node.

// peerPlaneIn is a partial update — an omitted field leaves the persisted
// value alone, so re-joining with a rotated token keeps the other credentials.
type peerPlaneIn struct {
	Endpoint   *string `json:"endpoint,omitempty" jsonschema:"the peer's tunnel server as host or host:port (default port 7000)"`
	Token      *string `json:"token,omitempty" jsonschema:"SENSITIVE - the peer's cluster.tunnel_token; read it there with 'outpost cluster control-plane token'"`
	STCPSecret *string `json:"stcp_secret,omitempty" jsonschema:"SENSITIVE - the peer's stcp_secret, which authorizes reaching its apiserver"`
	NodeToken  *string `json:"node_token,omitempty" jsonschema:"SENSITIVE - the k3s node token, read on the peer with 'outpost cluster token'"`
	APIPort    *int    `json:"api_port,omitempty" jsonschema:"local port this worker binds the joined apiserver on; defaults to 6443"`

	ClusterAgent   *bool    `json:"cluster_agent,omitempty" jsonschema:"run one real k3s-agent Node on the joined plane. Omit to leave the current selection alone; omitting both runtime fields on a host with no selection yields the agent-only default."`
	ClusterVirtual []string `json:"cluster_virtual,omitempty" jsonschema:"complete set of virtual-kubelet backends to register on the joined plane: vk-podman, vk-native, vk-ollama. Replaces the whole set; omit to leave the current selection alone."`
}

// peerPlaneOut is REDACTED by construction — there is no field that could
// carry a credential back out, so no future edit can leak one by accident.
type peerPlaneOut struct {
	OK             bool   `json:"ok"`
	Joined         bool   `json:"joined"`
	Endpoint       string `json:"endpoint,omitempty"`
	APIPort        int    `json:"api_port,omitempty"`
	HasToken       bool   `json:"has_token"`
	HasSTCPSecret  bool   `json:"has_stcp_secret"`
	HasNodeToken   bool   `json:"has_node_token"`
	ClusterEnabled bool   `json:"cluster_enabled"`
	RestartPending bool   `json:"restart_pending"`

	RuntimeAgent   bool     `json:"runtime_agent"`
	RuntimeVirtual []string `json:"runtime_virtual,omitempty"`
}

func toPeerPlaneOut(res admincore.PeerPlaneResult) peerPlaneOut {
	return peerPlaneOut{
		OK:             res.OK,
		Joined:         res.Joined,
		Endpoint:       res.Endpoint,
		APIPort:        res.APIPort,
		HasToken:       res.HasToken,
		HasSTCPSecret:  res.HasSTCPSecret,
		HasNodeToken:   res.HasNodeToken,
		ClusterEnabled: res.ClusterEnabled,
		RestartPending: res.RestartPending,
		RuntimeAgent:   res.RuntimeAgent,
		RuntimeVirtual: res.RuntimeVirtual,
	}
}

// nodeTokenOut carries a credential. It exists as its own shape, reachable
// only from the explicitly-named tool below, so the token can never ride along
// on a status read.
type nodeTokenOut struct {
	OK        bool   `json:"ok"`
	NodeToken string `json:"node_token"`
	Endpoint  string `json:"endpoint,omitempty"`
}

func (s *Server) registerPeerPlaneTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "outpost_cluster_peer_plane",
		Description: "Report which control plane this host is a node of: the peer endpoint when it joins a peer-hosted plane, " +
			"empty when it joins the cloudbox-hosted one. Credentials are reported as has_* presence flags only, never returned.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, peerPlaneOut, error) {
		res, err := s.core.PeerPlaneView()
		if err != nil {
			return apiErrResult[peerPlaneOut](err)
		}
		return nil, toPeerPlaneOut(res), nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "outpost_cluster_join_peer",
		Description: "Join a control plane hosted by a PEER outpost rather than by cloudbox. Requires the peer's tunnel endpoint and token; " +
			"supply stcp_secret and node_token too or the node will authenticate and then fail to reach the apiserver. " +
			"Enables cluster mode and triggers a restart. Runtime selection: pass cluster_agent / cluster_virtual to register " +
			"virtual-kubelet nodes on the joined plane (a peer plane supports them — it authenticates vk with k3s client certs); " +
			"omit both and the join selects the agent runtime only when nothing is selected already, never overwriting a prior choice. " +
			"Distinct from outpost_cluster_join, which re-enables cluster mode against whichever plane is already configured.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in peerPlaneIn) (*mcp.CallToolResult, peerPlaneOut, error) {
		res, err := s.core.JoinPeerPlane(admincore.PeerPlaneParams{
			Endpoint:   in.Endpoint,
			Token:      in.Token,
			STCPSecret: in.STCPSecret,
			NodeToken:  in.NodeToken,
			APIPort:    in.APIPort,
			Agent:      in.ClusterAgent,
			Virtual:    in.ClusterVirtual,
		})
		if err != nil {
			return apiErrResult[peerPlaneOut](err)
		}
		return nil, toPeerPlaneOut(res), nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "outpost_cluster_leave_peer",
		Description: "Stop joining a peer-hosted control plane and revert to the cloudbox-hosted one. Clears the peer endpoint, join token, " +
			"node token and STCP secret; leaves cluster mode itself ON (use outpost_cluster_leave to stop being a node). Triggers a restart.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, peerPlaneOut, error) {
		res, err := s.core.LeavePeerPlane()
		if err != nil {
			return apiErrResult[peerPlaneOut](err)
		}
		return nil, toPeerPlaneOut(res), nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "outpost_cluster_node_token",
		Description: "SENSITIVE OUTPUT - returns a credential. Read the k3s node token of the control plane hosted on THIS machine, " +
			"the value workers pass as node_token to outpost_cluster_join_peer. Treat it like the k3s node-token: do not echo it into logs " +
			"or transcripts. Fails when this host does not host a control plane.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, nodeTokenOut, error) {
		res, err := s.core.ControlPlaneNodeToken(ctx)
		if err != nil {
			return apiErrResult[nodeTokenOut](err)
		}
		return nil, nodeTokenOut{OK: res.OK, NodeToken: res.NodeToken, Endpoint: res.Endpoint}, nil
	})
}
