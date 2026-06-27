package mcpapi

import (
	"context"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

type meshExposeIn struct {
	Service string `json:"service" jsonschema:"Service name to expose over the mesh (e.g. rpc)"`
	Addr    string `json:"addr" jsonschema:"Local loopback address to bridge to (e.g. 127.0.0.1:50052)"`
}

type meshListenIn struct {
	PeerID    string `json:"peer_id" jsonschema:"The remote host's libp2p peer id (from outpost_mesh_status on that host)"`
	Service   string `json:"service" jsonschema:"The service name the remote host exposed (e.g. rpc)"`
	LocalAddr string `json:"local_addr,omitempty" jsonschema:"Local listen address (default 127.0.0.1:0 = ephemeral port)"`
}

type meshServiceIn struct {
	Service string `json:"service" jsonschema:"Service name"`
}

type meshAddrIn struct {
	Addr string `json:"addr" jsonschema:"Bound local listener address"`
}

type meshOKOut struct {
	OK bool `json:"ok"`
}

type meshListenOut struct {
	Addr string `json:"addr"`
}

type meshStatusOut struct {
	Status   *admincore.MeshStatusView `json:"status"`
	Forwards admincore.MeshForwardView `json:"forwards"`
}

func (s *Server) registerMeshTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_mesh_status",
		Description: "Show the libp2p mesh host status (peer id, listen addrs, connected peers) plus the forwarder state (exposed services + active forward listeners).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, meshStatusOut, error) {
		fwd, err := s.core.MeshForwards()
		if err != nil {
			return apiErrResult[meshStatusOut](err)
		}
		return nil, meshStatusOut{Status: s.core.MeshStatus(), Forwards: fwd}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_mesh_expose",
		Description: "Expose a local loopback service over the mesh under a name (worker side). Only exposed services are reachable by peers — never an arbitrary port. e.g. expose 'rpc' -> 127.0.0.1:50052.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in meshExposeIn) (*mcp.CallToolResult, meshOKOut, error) {
		if err := s.core.MeshExpose(in.Service, in.Addr); err != nil {
			return apiErrResult[meshOKOut](err)
		}
		return nil, meshOKOut{OK: true}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_mesh_unexpose",
		Description: "Stop exposing a mesh service (worker side).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in meshServiceIn) (*mcp.CallToolResult, meshOKOut, error) {
		if err := s.core.MeshUnexpose(in.Service); err != nil {
			return apiErrResult[meshOKOut](err)
		}
		return nil, meshOKOut{OK: true}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_mesh_listen",
		Description: "Open a local TCP listener that forwards to (peer_id, service) over the mesh (client/leader side). Returns the bound local address to point a client (e.g. llama-server --rpc) at.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in meshListenIn) (*mcp.CallToolResult, meshListenOut, error) {
		addr, err := s.core.MeshListen(in.PeerID, in.Service, in.LocalAddr)
		if err != nil {
			return apiErrResult[meshListenOut](err)
		}
		return nil, meshListenOut{Addr: addr}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_mesh_close_listen",
		Description: "Close a mesh forward listener by its bound local address (client/leader side).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in meshAddrIn) (*mcp.CallToolResult, meshOKOut, error) {
		if err := s.core.MeshCloseListen(in.Addr); err != nil {
			return apiErrResult[meshOKOut](err)
		}
		return nil, meshOKOut{OK: true}, nil
	})
}
