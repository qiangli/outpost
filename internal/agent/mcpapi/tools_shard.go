package mcpapi

import (
	"context"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type shardTriggerIn struct {
	Host  string `json:"host" jsonschema:"The paired peer host that should LEAD the shard (resolved to its mesh peer id via cloudbox)"`
	Model string `json:"model" jsonschema:"The model the leader should serve sharded across its ring (e.g. llama3.1:70b)"`
}

type shardStatusIn struct {
	Host string `json:"host,omitempty" jsonschema:"A paired peer host to ping over the mesh; empty = this local node's readiness"`
}

type shardOKOut struct {
	OK bool `json:"ok"`
}

type shardStatusOut struct {
	Status any `json:"status"`
}

func (s *Server) registerShardTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_shard_trigger",
		Description: "Tell a paired peer host to LEAD a shard for a model over the libp2p mesh (no ssh). The leader self-provisions and orchestrates its same-LAN ring. Fire-and-forget: leading is long-running and backgrounded on the leader.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in shardTriggerIn) (*mcp.CallToolResult, shardOKOut, error) {
		if err := s.core.ShardTrigger(ctx, in.Host, in.Model); err != nil {
			return apiErrResult[shardOKOut](err)
		}
		return nil, shardOKOut{OK: true}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_shard_status",
		Description: "Report a node's shard readiness over the mesh (models, memory budget, prima binaries present, active sharded model, ring members). Empty host = this local node; otherwise pings the named paired peer over the mesh — the fetch itself is the reachability proof, no ssh.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in shardStatusIn) (*mcp.CallToolResult, shardStatusOut, error) {
		rep, err := s.core.ShardStatus(ctx, in.Host)
		if err != nil {
			return apiErrResult[shardStatusOut](err)
		}
		return nil, shardStatusOut{Status: rep}, nil
	})
}
