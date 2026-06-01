// Default on-disk locations for the discovery package's persistent
// artifacts. Centralized here so the daemon, the CLI, the MCP tools,
// and the operator-facing `outpost peers` surfaces all agree on
// where to look.
//
// All paths sit under conf.DefaultCacheDir()/outpost/ — the same
// XDG-aware root the upgrade ledger and shell history use. Mode
// 0700 on the parent dir, 0600 on the files.
package discovery

import (
	"path/filepath"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// DefaultLedgerPath returns the canonical reachability-ledger path.
// Callers should pass this to OpenLedger.
func DefaultLedgerPath() (string, error) {
	base, err := conf.DefaultCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "reachability.jsonl"), nil
}

// DefaultObservationsPath returns the canonical temporal-observations
// model path. Callers pass this to OpenObservations.
func DefaultObservationsPath() (string, error) {
	base, err := conf.DefaultCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "peers_obs.json"), nil
}

// AppendLedgerEntry is the cmd-side convenience: open the ledger at
// the default path, append one entry, close. Errors are logged-not-
// fatal at the call site since a missing ledger entry is far less
// bad than a failed dial.
//
// Returns the path written to on success (for debug log lines) and
// any error encountered.
func AppendLedgerEntry(e ReachabilityEdge) (string, error) {
	path, err := DefaultLedgerPath()
	if err != nil {
		return "", err
	}
	l, err := OpenLedger(path)
	if err != nil {
		return path, err
	}
	return path, l.Append(e)
}
