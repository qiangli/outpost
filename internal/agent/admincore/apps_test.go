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

// TestUpsertApp_SSOSecretLifecycle locks in the auto-gen / preserve /
// clear pattern shared with ProvisioningToken. The cooperating app's
// bootstrap depends on the secret being minted exactly once and
// surviving subsequent edits — accidental rotation breaks the upstream
// verifier until the operator re-pastes.
func TestUpsertApp_SSOSecretLifecycle(t *testing.T) {
	core, cfgPath := newTestCore(t)

	// Initial upsert with TrustCloudIdentity=true mints both token and secret.
	if _, err := core.UpsertApp(AppUpsertParams{
		AppConfig: conf.AppConfig{Name: "myapp", Enabled: true, TrustCloudIdentity: true},
		URL:       "http://127.0.0.1:18080",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	first := fc.Apps[0].SSOSecret
	if first == "" {
		t.Fatal("SSOSecret not auto-generated when TrustCloudIdentity flipped on")
	}
	if fc.Apps[0].ProvisioningToken == "" {
		t.Fatal("ProvisioningToken regression — should still auto-gen")
	}

	// Re-upsert (caller didn't echo the secret back, which is the
	// normal admin-UI shape). Secret must be preserved.
	if _, err := core.UpsertApp(AppUpsertParams{
		AppConfig: conf.AppConfig{Name: "myapp", Enabled: true, TrustCloudIdentity: true},
		URL:       "http://127.0.0.1:18080",
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	fc, err = conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Apps[0].SSOSecret != first {
		t.Errorf("SSOSecret changed on re-upsert: %q → %q", first, fc.Apps[0].SSOSecret)
	}

	// GetSSOSecret returns the live value (matches CLI `outpost apps secret`).
	got, err := core.GetSSOSecret("myapp")
	if err != nil {
		t.Fatalf("GetSSOSecret: %v", err)
	}
	if got != first {
		t.Errorf("GetSSOSecret returned %q, want %q", got, first)
	}

	// RotateSSOSecret mints a new one and persists.
	rotated, err := core.RotateSSOSecret("myapp")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated == "" || rotated == first {
		t.Errorf("rotate returned %q (first=%q)", rotated, first)
	}
	fc, err = conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Apps[0].SSOSecret != rotated {
		t.Errorf("rotated secret not persisted: file=%q want %q", fc.Apps[0].SSOSecret, rotated)
	}

	// Flipping TrustCloudIdentity off clears the secret (off truly means off).
	if _, err := core.UpsertApp(AppUpsertParams{
		AppConfig: conf.AppConfig{Name: "myapp", Enabled: true, TrustCloudIdentity: false},
		URL:       "http://127.0.0.1:18080",
	}); err != nil {
		t.Fatalf("toggle off: %v", err)
	}
	fc, err = conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Apps[0].SSOSecret != "" {
		t.Errorf("SSOSecret survived TrustCloudIdentity=false: %q", fc.Apps[0].SSOSecret)
	}
	if fc.Apps[0].ProvisioningToken != "" {
		t.Errorf("ProvisioningToken survived TrustCloudIdentity=false: %q", fc.Apps[0].ProvisioningToken)
	}

	// GetSSOSecret now refuses (TrustCloudIdentity is off).
	if _, err := core.GetSSOSecret("myapp"); err == nil {
		t.Error("GetSSOSecret should fail when TrustCloudIdentity is off")
	}
	// Rotate refuses too.
	if _, err := core.RotateSSOSecret("myapp"); err == nil {
		t.Error("RotateSSOSecret should fail when TrustCloudIdentity is off")
	}
}

// TestUpsertApp_URLPathBecomesBasePath guards ce971c6f8ad9: `outpost apps
// add vlog --url http://127.0.0.1:22749/vlog/` must keep "/vlog" as the
// upstream base path instead of silently targeting the console root, and
// a path the scheme cannot honor is refused rather than dropped.
func TestUpsertApp_URLPathBecomesBasePath(t *testing.T) {
	core, cfgPath := newTestCore(t)
	got, err := core.UpsertApp(AppUpsertParams{
		AppConfig: conf.AppConfig{Name: "vlog", Enabled: true},
		URL:       "http://127.0.0.1:22749/vlog/",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got.BasePath != "/vlog" || got.Host != "127.0.0.1" || got.Port != 22749 {
		t.Fatalf("got base_path=%q host=%q port=%d, want /vlog 127.0.0.1 22749", got.BasePath, got.Host, got.Port)
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Apps[0].BasePath != "/vlog" {
		t.Errorf("persisted base_path=%q, want /vlog", fc.Apps[0].BasePath)
	}

	// Re-upsert at the root clears the base path.
	got, err = core.UpsertApp(AppUpsertParams{
		AppConfig: conf.AppConfig{Name: "vlog", Enabled: true},
		URL:       "http://127.0.0.1:22749/",
	})
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if got.BasePath != "" {
		t.Errorf("root URL kept base_path=%q", got.BasePath)
	}

	for _, tc := range []struct {
		name string
		p    AppUpsertParams
	}{
		{"tcp path", AppUpsertParams{AppConfig: conf.AppConfig{Name: "db", Enabled: true}, URL: "tcp://127.0.0.1:5432/db"}},
		{"query", AppUpsertParams{AppConfig: conf.AppConfig{Name: "q", Enabled: true}, URL: "http://127.0.0.1:1/x?y=1"}},
		{"base_path on unix", AppUpsertParams{AppConfig: conf.AppConfig{Name: "s", Enabled: true, Scheme: "unix", Socket: "/tmp/s.sock", BasePath: "/x"}}},
		{"dot segment", AppUpsertParams{AppConfig: conf.AppConfig{Name: "d", Enabled: true, Scheme: "http", Port: 1, BasePath: "/a/../b"}}},
	} {
		var ae *APIError
		if _, err := core.UpsertApp(tc.p); !errors.As(err, &ae) || ae.Status != 400 {
			t.Errorf("%s: want 400 APIError, got %v", tc.name, err)
		}
	}
}
