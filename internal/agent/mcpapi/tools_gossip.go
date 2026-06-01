// SWIM gossip MCP surface — roadmap item #17.
//
// One tool: outpost_gossip_edges. Returns the live memberlist
// (alive / suspect / dead) for the local gossip mesh. Agents can
// use this to see which fleet members this outpost can reach via
// gossip (independent of cloudbox), useful for diagnosing
// route-to recommendations.

package mcpapi

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type gossipEdgesOut struct {
	Members any `json:"members"`
}

func (s *Server) registerGossipTools() {
	if s.gossipMembersFn == nil {
		// Tool not registered when gossip is off — avoids "tool
		// exists but always errors" noise for agentic callers.
		return
	}
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_gossip_edges",
		Description: "Live SWIM gossip membership for this outpost. Returns one row per known fleet member with state (alive/suspect/dead/left) and the addr:port memberlist tracks. Use to diagnose `outpost peers route-to` suggestions — peers visible here are reachable through the gossip mesh; peers absent are either unpaired or partitioned off.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, gossipEdgesOut, error) {
		if s.gossipMembersFn == nil {
			return apiErrResult[gossipEdgesOut](errors.New("gossip disabled"))
		}
		return nil, gossipEdgesOut{Members: s.gossipMembersFn()}, nil
	})
}
