package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/upgrade"
)

// newLocalUpgrade initializes local recovery for every daemon, including hosts
// that have never paired. Only fleet push/pull transport requires cloud credentials.
func newLocalUpgrade(cfgPath string, fc *conf.FileConfig, restart func()) (*upgrade.Worker, *upgrade.Ledger, string, error) {
	cacheDir, _ := conf.ResolveCacheDir()
	upgradeConfirmPath := upgrade.PendingConfirmPath(cacheDir)
	// Report "healthy=false" to cloudbox while a self-upgrade
	// is pending confirmation (the watchdog marker is present),
	// so the fleet health-gate sees an unconfirmed host. Hooked
	// (not a direct call) to avoid an agent→upgrade import cycle.
	if cp := upgradeConfirmPath; cp != "" {
		agent.HealthyProbe = func() bool {
			pc, _ := upgrade.ReadPendingConfirm(cp)
			return pc == nil
		}
	}
	ledgerPath := ""
	if cacheDir != "" {
		ledgerPath = filepath.Join(cacheDir, "upgrade.log")
	}
	upgradeLedger := upgrade.NewLedger(ledgerPath)
	pendingPath := upgrade.PendingPath(cacheDir)
	exe, _ := os.Executable()
	// Drop any <exe>.replaced-* siblings left behind by a
	// prior Windows-swap run. No-op on Unix. Idempotent.
	upgrade.CleanupStaleSwaps(exe)
	upgradeWorker, err := upgrade.NewWorker(upgrade.Options{
		State: func() upgrade.StateSnapshot {
			// Re-read the current FileConfig so a just-toggled
			// update_mode takes effect on the next /admin/upgrade
			// POST without a daemon restart.
			cur, _ := conf.LoadFile(cfgPath)
			if cur == nil {
				cur = fc
			}
			return upgrade.StateSnapshot{
				UpdateMode:    cur.UpdateModeName(),
				CurrentCommit: agent.ReadBuildInfo().ShortCommit(),
				CurrentDirty:  agent.ReadBuildInfo().Dirty,
				BinaryPath:    exe,
				PendingPath:   pendingPath,
			}
		},
		Restart:        restart,
		Ledger:         upgradeLedger,
		ConfirmPath:    upgradeConfirmPath,
		QuarantinePath: upgrade.QuarantinePath(cacheDir),
	})
	if err != nil {
		return nil, nil, "", fmt.Errorf("upgrade worker: %w", err)
	}
	return upgradeWorker, upgradeLedger, upgradeConfirmPath, nil
}
