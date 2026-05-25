package mcpapi

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type restartOut struct {
	OK             bool `json:"ok"`
	RestartPending bool `json:"restart_pending"`
}

type rotateMCPOut struct {
	OK              bool   `json:"ok"`
	NewBearerToken  string `json:"new_bearer_token" jsonschema:"The freshly-minted token. The OLD token stops authenticating immediately — update your .mcp.json before the next call."`
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
		if s.rotateFn == nil {
			return nil, rotateMCPOut{}, errors.New("rotation not configured (daemon misconfigured)")
		}
		newTok, err := s.rotateFn()
		if err != nil {
			return nil, rotateMCPOut{}, err
		}
		s.token = newTok
		return nil, rotateMCPOut{OK: true, NewBearerToken: newTok}, nil
	})
}
