package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/admincore"
)

type upsertOutboundIn struct {
	Path       string `json:"path" jsonschema:"Local mount identifier; reachable at http://127.0.0.1:17777/<path>/ for http schemes, or 127.0.0.1:<local_port> for tcp/ssh"`
	Name       string `json:"name,omitempty" jsonschema:"Remote app name (required for http/tcp; ignored for ssh)"`
	Host       string `json:"host" jsonschema:"Remote outpost's name as registered in cloudbox"`
	User       string `json:"user" jsonschema:"OS user on the remote outpost (used at /elevate time)"`
	Scheme     string `json:"scheme,omitempty" jsonschema:"http (default) | tcp | ssh"`
	LocalPort  int    `json:"local_port,omitempty" jsonschema:"Required for tcp/ssh; the local listener port"`
	TTLSeconds int64  `json:"ttl_seconds,omitempty" jsonschema:"Per-mount elevate-cookie absolute-expiry override; 0 = cloudbox default; math.MaxInt64 = no cap"`
}

type byPathIn struct {
	Path string `json:"path" jsonschema:"Outbound mount path"`
}

type connectOutboundIn struct {
	Path     string `json:"path" jsonschema:"Outbound mount path"`
	Password string `json:"password" jsonschema:"OS password on the remote host. AGENTS MUST ask the user for this every call — do not cache. Used once to clear cloudbox's /elevate gate; the resulting matrix_elev cookie lives in this outpost's memory only."`
}

type listOutboundOut struct {
	Outbound []agent.OutboundView `json:"outbound"`
}

type outboundSuggestionsOut struct {
	Suggestions []admincore.OutboundSuggestion `json:"suggestions"`
}

func (s *Server) registerOutboundTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_list_outbound",
		Description: "List outbound mounts (local paths/ports that proxy through cloudbox to remote outposts). See also resource outpost://outbound.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, listOutboundOut, error) {
		return nil, listOutboundOut{Outbound: s.core.ListOutbound()}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_upsert_outbound",
		Description: "Add or update an outbound mount. Live mutation — no restart. After upsert, call outpost_connect_outbound to clear the elevate gate with the user's OS password.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in upsertOutboundIn) (*mcp.CallToolResult, okOut, error) {
		if err := s.core.UpsertOutbound(admincore.OutboundParams{
			Path:       in.Path,
			Name:       in.Name,
			Host:       in.Host,
			User:       in.User,
			Scheme:     in.Scheme,
			LocalPort:  in.LocalPort,
			TTLSeconds: in.TTLSeconds,
		}); err != nil {
			return apiErrResult[okOut](err)
		}
		return nil, okOut{OK: true}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_delete_outbound",
		Description: "Remove an outbound mount by path. Idempotent.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in byPathIn) (*mcp.CallToolResult, okOut, error) {
		if err := s.core.DeleteOutbound(in.Path); err != nil {
			return apiErrResult[okOut](err)
		}
		return nil, okOut{OK: true}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_connect_outbound",
		Description: "Clear the cloudbox /elevate gate for an outbound mount using the user's OS password on the remote host. Captures matrix_elev cookie in memory and starts the 4-minute pinger. Human-in-the-loop: agents must ask the user for the password on every call; do not cache.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in connectOutboundIn) (*mcp.CallToolResult, okOut, error) {
		if err := s.core.ConnectOutbound(in.Path, in.Password); err != nil {
			return apiErrResult[okOut](err)
		}
		return nil, okOut{OK: true}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_disconnect_outbound",
		Description: "Drop the matrix_elev cookie for an outbound mount. Idempotent.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in byPathIn) (*mcp.CallToolResult, okOut, error) {
		if err := s.core.DisconnectOutbound(in.Path); err != nil {
			return apiErrResult[okOut](err)
		}
		return nil, okOut{OK: true}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_suggest_outbound",
		Description: "Fetch the catalog of (host, app) pairs the caller is allowed to mount, from cloudbox's /api/v1/hosts. Returns ServiceUnavailable when the outpost isn't paired.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, outboundSuggestionsOut, error) {
		out, err := s.core.OutboundSuggestions(ctx)
		if err != nil {
			return apiErrResult[outboundSuggestionsOut](err)
		}
		return nil, outboundSuggestionsOut{Suggestions: out}, nil
	})
}
