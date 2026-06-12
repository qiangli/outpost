package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/peerstatus"
)

type restartOut struct {
	OK             bool `json:"ok"`
	RestartPending bool `json:"restart_pending"`
}

type peersStatusOut struct {
	Peers []peerstatus.Peer `json:"peers"`
}

type rotateMCPOut struct {
	OK             bool   `json:"ok"`
	NewBearerToken string `json:"new_bearer_token" jsonschema:"The freshly-minted token. The OLD token stops authenticating immediately — update your .mcp.json before the next call."`
}

func (s *Server) registerLifecycleTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_restart",
		Description: "Trigger a self-restart of the outpost daemon. Useful when an operator wants to reload built-in routes without touching a toggle. Returns restart_pending=true; poll outpost://status until configured returns to true.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, restartOut, error) {
		s.core.ScheduleRestart()
		return nil, restartOut{OK: true, RestartPending: true}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_rotate_mcp_token",
		Description: "Mint a fresh MCP bearer token, persist it, and start accepting it for subsequent calls. The previous token stops working IMMEDIATELY — surface the new value to the operator so they can update their .mcp.json before the next request.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, rotateMCPOut, error) {
		newTok, err := s.Rotate()
		if err != nil {
			return nil, rotateMCPOut{}, err
		}
		return nil, rotateMCPOut{OK: true, NewBearerToken: newTok}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_peers_status",
		Description: "List the paired hosts this account can see (its owned hosts + hosts shared with it) with online status, a same-LAN/remote location hint (relative to this host's network), and the build/OS/arch details each host last reported. Queries cloudbox; only works on a paired host.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, peersStatusOut, error) {
		peers, err := s.core.PeerStatus(ctx)
		if err != nil {
			return nil, peersStatusOut{}, err
		}
		return nil, peersStatusOut{Peers: peers}, nil
	})
}
