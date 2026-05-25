package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// pairIn mirrors admincore.PairParams with jsonschema-flavored field
// descriptions agent tools surface to their users.
type pairIn struct {
	Server     string `json:"server,omitempty" jsonschema:"Portal URL; defaults to https://ai.dhnt.io when empty"`
	Code       string `json:"code" jsonschema:"One-time pairing code from the portal"`
	Name       string `json:"name" jsonschema:"Host name to display in the portal"`
	Title      string `json:"title,omitempty" jsonschema:"Optional human-readable subtitle"`
	AuthURL    string `json:"auth_url,omitempty" jsonschema:"Optional external auth endpoint; overrides OS PAM"`
	ClientOnly bool   `json:"client_only,omitempty" jsonschema:"Pair as credential-only outpost — no inbound tunnel"`
}

type pairOut struct {
	OK             bool   `json:"ok"`
	AgentName      string `json:"agent_name"`
	RestartPending bool   `json:"restart_pending" jsonschema:"True when the daemon will restart to apply the change; poll outpost://status until it returns configured=true"`
}

type emptyIn struct{}

type unpairOut struct {
	OK             bool `json:"ok"`
	RestartPending bool `json:"restart_pending"`
}

func (s *Server) registerPairingTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_pair",
		Description: "Pair this outpost with a cloudbox portal via a one-time code. Returns restart_pending=true; the daemon re-execs to bring up the matrix tunnel.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in pairIn) (*mcp.CallToolResult, pairOut, error) {
		res, err := s.core.Pair(ctx, admincore.PairParams{
			Server: in.Server, Code: in.Code, Name: in.Name,
			Title: in.Title, AuthURL: in.AuthURL, ClientOnly: in.ClientOnly,
		})
		if err != nil {
			return apiErrResult[pairOut](err)
		}
		return nil, pairOut{OK: res.OK, AgentName: res.AgentName, RestartPending: res.RestartPending}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_unpair",
		Description: "Clear the portal pairing (AgentName, Token, AccessToken). Apps, outbound mounts, and builtin toggles are preserved. Returns restart_pending=true.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, unpairOut, error) {
		res, err := s.core.Unpair()
		if err != nil {
			return apiErrResult[unpairOut](err)
		}
		return nil, unpairOut{OK: res.OK, RestartPending: res.RestartPending}, nil
	})
}

// apiErrResult turns an admincore.APIError into an MCP CallToolResult
// with IsError=true and the human message as a text content block. The
// (zero, nil) on the latter two return values is fine — MCP SDK only
// inspects the *CallToolResult when err is nil.
func apiErrResult[T any](err error) (*mcp.CallToolResult, T, error) {
	var zero T
	if ae := admincore.AsAPIError(err); ae != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: ae.Msg}},
		}, zero, nil
	}
	// Non-APIError → surface as a JSON-RPC error so agent tools see
	// "tool call failed" rather than a successful-with-IsError result.
	return nil, zero, err
}
