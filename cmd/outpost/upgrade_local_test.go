package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/upgrade"
)

func TestLocalUpgradeAvailableBeforePairing(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	fc := &conf.FileConfig{}
	if err := conf.SaveFile(cfgPath, fc); err != nil {
		t.Fatal(err)
	}
	oldProbe := agent.HealthyProbe
	t.Cleanup(func() { agent.HealthyProbe = oldProbe })
	restarted := false
	worker, ledger, confirmPath, err := newLocalUpgrade(cfgPath, fc, func() { restarted = true })
	if err != nil {
		t.Fatal(err)
	}
	if worker == nil || ledger == nil || confirmPath == "" {
		t.Fatal("unpaired daemon lacks local recovery state")
	}
	result, err := worker.Rollback(context.Background())
	if err != nil || result.Status != "no_previous" {
		t.Fatalf("fresh unpaired rollback = %+v, %v; want no_previous", result, err)
	}
	if restarted {
		t.Fatal("rollback without a previous binary restarted daemon")
	}
	if !agent.HealthyProbe() {
		t.Fatal("fresh daemon reported unconfirmed")
	}
	if err := upgrade.WritePendingConfirm(confirmPath, upgrade.NewPendingConfirm("test-release", "old", "new", "outpost", "outpost.previous")); err != nil {
		t.Fatal(err)
	}
	if agent.HealthyProbe() {
		t.Fatal("unpaired upgrade marker was not reflected in health")
	}
	if err := upgrade.ClearPendingConfirm(confirmPath); err != nil {
		t.Fatal(err)
	}
	if !agent.HealthyProbe() {
		t.Fatal("confirmed unpaired daemon remained unhealthy")
	}
}
