package admincore

import (
	"context"
	"os"
	"time"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/upgrade"
)

// UpgradeSource describes where the currently-running binary came
// from. Derived by walking the ledger backwards from the most recent
// swap_done entry; nil when no swap has ever run on this host (the
// binary is whatever the operator manually installed).
type UpgradeSource struct {
	Kind      string    `json:"kind"`            // "cloudbox" / "cli-url" / "cli-local" / "unknown"
	URL       string    `json:"url,omitempty"`   // GitHub release URL for cloudbox / cli-url paths
	ReleaseID string    `json:"release_id,omitempty"`
	At        time.Time `json:"at,omitzero"`
}

// UpgradeOverview is the wire shape rendered into the admin UI's
// Update tab. One round-trip surfaces everything the operator
// usually wants to see when thinking about versions: what build is
// running, where it came from, when it landed, whether a pending
// envelope is queued, and the recent ledger.
//
// All fields are zero-valued / nil / empty on unpaired hosts (no
// Upgrader was threaded into Deps). The handler 404s before this
// runs on those hosts — but the struct stays well-formed either way.
type UpgradeOverview struct {
	Build             agent.BuildInfo       `json:"build"`
	BinaryPath        string                `json:"binary_path,omitempty"`
	UpdateMode        string                `json:"update_mode"`
	RollbackAvailable bool                  `json:"rollback_available"`
	CurrentSource     *UpgradeSource        `json:"current_source,omitempty"`
	Pending           *upgrade.Envelope     `json:"pending,omitempty"`
	History           []upgrade.LedgerEntry `json:"history"`
}

// AttachUpgrade injects the upgrade Worker + Ledger after admincore
// construction. The Worker's Restart closure normally points at the
// admincore Server's ScheduleRestart, which means worker construction
// needs the Server to already exist — so we can't pass them through
// the initial Deps. Setter pattern instead; safe to call once at
// startup, no concurrent readers yet.
func (s *Server) AttachUpgrade(worker *upgrade.Worker, ledger *upgrade.Ledger) {
	s.deps.Upgrader = worker
	s.deps.UpgradeLedger = ledger
}

// UpgradeOverview returns the consolidated payload for the Update
// tab. History is bounded to the most recent 20 entries — operators
// don't typically need more than that, and the JSONL ledger is
// unbounded in principle but rare in practice.
func (s *Server) UpgradeOverview() (UpgradeOverview, error) {
	fc, err := s.loadConfig()
	if err != nil {
		return UpgradeOverview{}, err
	}
	exe, _ := os.Executable()
	out := UpgradeOverview{
		Build:      agent.ReadBuildInfo(),
		BinaryPath: exe,
		UpdateMode: fc.UpdateModeName(),
		History:    []upgrade.LedgerEntry{},
	}

	// Rollback availability: a sibling outpost.previous file from a
	// prior swap. The Worker.Rollback path validates it (probe before
	// swap) so the SPA's button stays honest — we only show it when
	// there's actually a target.
	if exe != "" {
		if _, statErr := os.Stat(exe + ".previous"); statErr == nil {
			out.RollbackAvailable = true
		}
	}

	if s.deps.Upgrader != nil {
		if pending, _ := s.deps.Upgrader.LoadPending(); pending != nil {
			out.Pending = pending
		}
	}
	if s.deps.UpgradeLedger != nil {
		entries, lerr := s.deps.UpgradeLedger.Tail(20)
		if lerr == nil && entries != nil {
			out.History = entries
			out.CurrentSource = deriveCurrentSource(entries)
		}
	}
	return out, nil
}

// deriveCurrentSource walks the ledger newest-to-oldest looking for
// the most recent swap_done. The matching "received" entry (same
// release_id) carries the URL — that's the source for cloudbox-driven
// upgrades. CLI-driven swaps have Detail "outpost upgrade (CLI)"
// without a matching received entry, distinguished by that string.
// Returns nil when no swap has ever run on this host (initial install).
func deriveCurrentSource(entries []upgrade.LedgerEntry) *UpgradeSource {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Step != "swap_done" {
			continue
		}
		// Look earlier for a matching received entry.
		for j := i - 1; j >= 0; j-- {
			rcv := entries[j]
			if rcv.Step == "received" && rcv.ReleaseID == e.ReleaseID && rcv.ReleaseID != "" {
				return &UpgradeSource{
					Kind:      "cloudbox",
					URL:       rcv.URL,
					ReleaseID: rcv.ReleaseID,
					At:        e.At,
				}
			}
		}
		// No matching received → CLI path. Detail string distinguishes
		// the two CLI flavors today; future flavors can append more
		// markers without breaking this.
		if e.Detail == "outpost upgrade (CLI)" {
			return &UpgradeSource{Kind: "cli-local", At: e.At}
		}
		return &UpgradeSource{Kind: "unknown", At: e.At}
	}
	return nil
}

// ApplyPendingUpgrade — admincore-side wrapper around the Worker's
// LoadPending + Apply with Force=true. Same flow as the MCP tool
// outpost_apply_pending; exposed here so the adminui /api/upgrade/
// apply route doesn't need its own copy of the worker handle.
func (s *Server) ApplyPendingUpgrade(ctx context.Context) (upgrade.Result, error) {
	if s.deps.Upgrader == nil {
		return upgrade.Result{Status: "invalid", Detail: "no upgrade worker on this host (unpaired?)"}, nil
	}
	env, err := s.deps.Upgrader.LoadPending()
	if err != nil {
		return upgrade.Result{}, err
	}
	if env == nil {
		return upgrade.Result{Status: "no_pending", Detail: "no upgrade.pending.json on disk"}, nil
	}
	env.Force = true
	return s.deps.Upgrader.Apply(ctx, *env), nil
}

// RollbackUpgrade — admincore-side wrapper around Worker.Rollback.
// Same return shape as the MCP tool outpost_rollback.
func (s *Server) RollbackUpgrade(ctx context.Context) (upgrade.RollbackResult, error) {
	if s.deps.Upgrader == nil {
		return upgrade.RollbackResult{Status: "invalid", Detail: "no upgrade worker on this host (unpaired?)"}, nil
	}
	return s.deps.Upgrader.Rollback(ctx)
}
