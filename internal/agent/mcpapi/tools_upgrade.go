package mcpapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/upgrade"
)

type rollbackOut struct {
	OK         bool   `json:"ok"`
	Status     string `json:"status,omitempty"`
	Detail     string `json:"detail,omitempty"`
	FromCommit string `json:"from_commit,omitempty"`
	ToCommit   string `json:"to_commit,omitempty"`
}

type upgradeHistoryOut struct {
	Entries []upgrade.LedgerEntry `json:"entries"`
}

type upgradeHistoryIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"Maximum number of newest entries to return. 0 means all."`
}

// registerUpgradeTools wires the cloudbox-pushed upgrade surface into
// MCP. Only registered when main.go threaded an upgrade.Worker —
// unpaired hosts skip these tools entirely so cloudbox-driven
// orchestration can't even attempt to drive them.
func (s *Server) registerUpgradeTools() {
	if s.upgrader == nil {
		return
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_rollback",
		Description: "Restore the previously-running outpost binary (outpost.previous) over the current one and re-exec. Refuses when no rollback candidate is on disk (the daemon has never been upgraded) or when an upgrade is currently in flight.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, rollbackOut, error) {
		res, err := s.upgrader.Rollback(ctx)
		if err != nil {
			return nil, rollbackOut{}, err
		}
		out := rollbackOut{
			OK:         res.Status == "",
			Status:     res.Status,
			Detail:     res.Detail,
			FromCommit: res.FromCommit,
			ToCommit:   res.Previous.Short(),
		}
		if res.Status != "" && res.Status != "no_previous" && res.Status != "in_flight" {
			return nil, out, fmt.Errorf("rollback refused: %s", res.Detail)
		}
		return nil, out, nil
	})

	if s.ledger != nil {
		mcp.AddTool(s.mcp, &mcp.Tool{
			Name:        "outpost_upgrade_history",
			Description: "Return the upgrade ledger for this host — one JSON object per phase of every cloudbox-pushed or CLI-driven upgrade (received, stage_failed, swap_done, rollback, etc). Newest entries last. Pass {limit: N} to bound the response.",
		}, func(_ context.Context, _ *mcp.CallToolRequest, in upgradeHistoryIn) (*mcp.CallToolResult, upgradeHistoryOut, error) {
			entries, err := s.ledger.Tail(in.Limit)
			if err != nil {
				return nil, upgradeHistoryOut{}, err
			}
			if entries == nil {
				entries = []upgrade.LedgerEntry{}
			}
			return nil, upgradeHistoryOut{Entries: entries}, nil
		})
	}
}

// errNoUpgrader is the sentinel for callers that try to drive
// upgrade tools on an unpaired host. Currently unused — the tool is
// simply not registered — but exported for tests that want to
// distinguish "tool absent" from "tool refused."
var errNoUpgrader = errors.New("upgrade surface not available on this host (unpaired?)")
