package admincore

import (
	"errors"
	"slices"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func TestSetAppEnabled_TogglesAndPersists(t *testing.T) {
	core, cfgPath := newTestCore(t)

	// Seed an enabled app via the upsert path so we exercise the full
	// validation pipeline.
	if _, err := core.UpsertApp(AppUpsertParams{
		AppConfig: conf.AppConfig{Name: "myapp", Enabled: true},
		URL:       "http://127.0.0.1:18080",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Stop it.
	got, err := core.SetAppEnabled("myapp", false)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got.Enabled {
		t.Errorf("returned Enabled=true, want false")
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Apps[0].Enabled {
		t.Errorf("persisted Enabled=true, want false")
	}
	// Disabling unregisters from the live registry — Get* on the
	// registry should report it's gone (or the entry is still there
	// but not proxying; either way Lookup returns ok=false for
	// disabled names because UpsertApp's gate doesn't re-register).
	if slices.Contains(core.deps.Apps.Names(), "myapp") {
		t.Error("live registry still has myapp after disable")
	}

	// Start it back.
	got, err = core.SetAppEnabled("myapp", true)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !got.Enabled {
		t.Errorf("returned Enabled=false, want true")
	}
	if !slices.Contains(core.deps.Apps.Names(), "myapp") {
		t.Error("live registry missing myapp after re-enable")
	}
}

func TestSetAppEnabled_IdempotentNoChange(t *testing.T) {
	core, _ := newTestCore(t)
	if _, err := core.UpsertApp(AppUpsertParams{
		AppConfig: conf.AppConfig{Name: "stable", Enabled: true},
		URL:       "http://127.0.0.1:18080",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Same state twice — should still report Enabled=true and not
	// error.
	got, err := core.SetAppEnabled("stable", true)
	if err != nil {
		t.Fatalf("idempotent: %v", err)
	}
	if !got.Enabled {
		t.Errorf("Enabled = false, want true")
	}
}

func TestSetAppEnabled_UnknownName404(t *testing.T) {
	core, _ := newTestCore(t)
	_, err := core.SetAppEnabled("nope", false)
	if err == nil {
		t.Fatal("expected error for unknown app")
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Errorf("want 404 APIError, got %v", err)
	}
}
