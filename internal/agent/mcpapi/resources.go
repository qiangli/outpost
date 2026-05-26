package mcpapi

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Resources are read-only snapshots agent tools fetch to reason about
// the daemon's state. Unlike tools, resources are idempotent — calling
// the same URI twice without mutations returns the same payload.
//
// Resources we expose:
//   - outpost://status   — paired/not, agent name, server, OS user
//   - outpost://config   — full SafeView (redacted FileConfig)
//   - outpost://apps     — registered custom apps
//   - outpost://outbound — outbound mounts + live state
//
// MCP ResourceContents support text or binary; we always emit one
// text block with the JSON payload, which the SDK exposes to clients
// as a single resource read.
func (s *Server) registerReadOnlyResources() {
	s.addJSONResource("outpost://status",
		"Outpost status",
		"Paired/not, agent name, server URL, current OS user. Cheap, refresh frequently when polling for a post-restart 'configured=true'.",
		func(ctx context.Context) (any, error) { return s.core.Status() })

	s.addJSONResource("outpost://config",
		"Outpost full config (redacted)",
		"Full FileConfig view with secrets stripped — Token, AccessToken, ProvisioningToken, Cluster.Token, Cluster.CA never leave the daemon. Includes live built-in detection (podman/ollama Available + Target), LLM pool diagnostic, outbound mount status.",
		func(ctx context.Context) (any, error) { return s.core.SafeView() })

	s.addJSONResource("outpost://apps",
		"Registered custom apps",
		"The Apps slice from FileConfig. For the live registry's view (which includes runtime metadata cloudbox sees through /apps), call cloudbox's API.",
		func(ctx context.Context) (any, error) {
			apps, err := s.core.ListApps()
			if err != nil {
				return nil, err
			}
			return map[string]any{"apps": apps}, nil
		})

	s.addJSONResource("outpost://outbound",
		"Outbound mounts + live state",
		"Per-mount status including matrix_elev cookie freshness, ttl_seconds remaining, and connected/disconnected. Mirrors the /api/outbound response shape.",
		func(ctx context.Context) (any, error) {
			return map[string]any{"outbound": s.core.ListOutbound()}, nil
		})

	if s.ledger != nil {
		s.addJSONResource("outpost://upgrade-history",
			"Upgrade ledger",
			"JSONL-decoded history of cloudbox-pushed and CLI-driven upgrades on this host. One entry per phase (received, stage_failed, swap_done, rollback, etc). Empty {entries:[]} when no upgrades have ever run.",
			func(ctx context.Context) (any, error) {
				entries, err := s.ledger.Tail(0)
				if err != nil {
					return nil, err
				}
				if entries == nil {
					return map[string]any{"entries": []any{}}, nil
				}
				return map[string]any{"entries": entries}, nil
			})
	}
}

// addJSONResource wires one read-only resource that always renders a
// single JSON content block. Caller supplies the URI, name,
// description, and a closure that returns the value to marshal.
func (s *Server) addJSONResource(uri, name, desc string, fetch func(ctx context.Context) (any, error)) {
	res := &mcp.Resource{
		URI:         uri,
		Name:        name,
		Description: desc,
		MIMEType:    "application/json",
	}
	s.mcp.AddResource(res, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		v, err := fetch(ctx)
		if err != nil {
			return nil, err
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      uri,
				MIMEType: "application/json",
				Text:     string(b),
			}},
		}, nil
	})
}
