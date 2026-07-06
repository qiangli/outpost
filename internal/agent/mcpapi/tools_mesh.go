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

type meshServicesOut struct {
	Services []admincore.MeshServiceView `json:"services"`
}

type meshDialIn struct {
	Service   string `json:"service" jsonschema:"The mesh service name to dial — resolved to a peer via the registry (e.g. git, registry)"`
	LocalAddr string `json:"local_addr,omitempty" jsonschema:"Local listen address (default 127.0.0.1:0 = ephemeral)"`
}

type meshDialOut struct {
	Addr string `json:"addr"`
	Host string `json:"host"`
}

type meshConsumeIn struct {
	Service   string `json:"service" jsonschema:"The remote mesh service name to reach (e.g. git)"`
	PeerID    string `json:"peer_id" jsonschema:"The libp2p peer id of the host exposing the service"`
	LocalAddr string `json:"local_addr,omitempty" jsonschema:"Fixed local listen address so the forward is stable across restarts (e.g. 127.0.0.1:31880)"`
}

type meshConsumeRmIn struct {
	Service   string `json:"service" jsonschema:"The service name of the consume to remove"`
	LocalAddr string `json:"local_addr,omitempty" jsonschema:"The local address of the consume to remove (must match)"`
}

type meshConsumeSetOut struct {
	Addr string `json:"addr"`
}

type meshConsumesOut struct {
	Consumes []admincore.MeshConsumeView `json:"consumes"`
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

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_mesh_service_set",
		Description: "Persistently expose a local loopback service over the mesh (the wrap harness): saved to config + exposed live, and auto-exposed on every boot. e.g. set 'git' -> 127.0.0.1:3000. Use this (not mesh_expose) for services that should survive restarts.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in meshExposeIn) (*mcp.CallToolResult, meshOKOut, error) {
		if err := s.core.MeshServiceUpsert(in.Service, in.Addr); err != nil {
			return apiErrResult[meshOKOut](err)
		}
		return nil, meshOKOut{OK: true}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_mesh_service_rm",
		Description: "Remove a persistently-exposed mesh service (un-exposes it live + drops it from config).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in meshServiceIn) (*mcp.CallToolResult, meshOKOut, error) {
		if err := s.core.MeshServiceDelete(in.Service); err != nil {
			return apiErrResult[meshOKOut](err)
		}
		return nil, meshOKOut{OK: true}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_mesh_services",
		Description: "List the persistently-exposed mesh services (the wrap-harness config).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, meshServicesOut, error) {
		svcs, err := s.core.MeshServices()
		if err != nil {
			return apiErrResult[meshServicesOut](err)
		}
		return nil, meshServicesOut{Services: svcs}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_mesh_dial",
		Description: "Resolve which peer exposes a named mesh service (the service registry) and open a local forward listener to it — the zero-config consume side. Returns the local address to point a client at + the chosen peer host. e.g. dial 'git'.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in meshDialIn) (*mcp.CallToolResult, meshDialOut, error) {
		addr, host, err := s.core.MeshDial(in.Service, in.LocalAddr)
		if err != nil {
			return apiErrResult[meshDialOut](err)
		}
		return nil, meshDialOut{Addr: addr, Host: host}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_mesh_consume_set",
		Description: "Persistently CONSUME a remote mesh service by peer id (the dial side of the wrap harness): saved to config + established live, and re-established on every boot by peer id (no cloudbox resolve, so it's stable across restarts). Use this for a cross-host dependency that must survive a restart — e.g. this node's act_runner reaching a loom forge on another host. Pass a fixed local_addr so the consuming config stays stable.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in meshConsumeIn) (*mcp.CallToolResult, meshConsumeSetOut, error) {
		addr, err := s.core.MeshConsumeUpsert(in.Service, in.PeerID, in.LocalAddr)
		if err != nil {
			return apiErrResult[meshConsumeSetOut](err)
		}
		return nil, meshConsumeSetOut{Addr: addr}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_mesh_consume_rm",
		Description: "Remove a persistent mesh consume (closes the live listener + drops it from config). Keyed by (service, local_addr).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in meshConsumeRmIn) (*mcp.CallToolResult, meshOKOut, error) {
		if err := s.core.MeshConsumeDelete(in.Service, in.LocalAddr); err != nil {
			return apiErrResult[meshOKOut](err)
		}
		return nil, meshOKOut{OK: true}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_mesh_consumes",
		Description: "List the persistent mesh consumes (the dial-side wrap-harness config).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, meshConsumesOut, error) {
		cons, err := s.core.MeshConsumes()
		if err != nil {
			return apiErrResult[meshConsumesOut](err)
		}
		return nil, meshConsumesOut{Consumes: cons}, nil
	})
}
