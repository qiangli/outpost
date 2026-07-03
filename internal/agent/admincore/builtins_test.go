package admincore

import (
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func TestSetBuiltinsPersistsGenericBashyServices(t *testing.T) {
	core, cfgPath := newTestCore(t)
	services := []conf.BashyService{{
		Name:         "loom",
		Enabled:      true,
		AppName:      "loom",
		AppPort:      31880,
		RequireLogin: true,
		MeshService:  "git",
	}}
	res, err := core.SetBuiltins(BuiltinsParams{BashyServices: services})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || !res.RestartPending {
		t.Fatalf("unexpected result: %+v", res)
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.BashyServices) != 1 || fc.BashyServices[0].Name != "loom" || !fc.BashyServices[0].Enabled {
		t.Fatalf("bashy service not persisted: %+v", fc.BashyServices)
	}
}

func TestSetBuiltinsPersistsBashyVersionPin(t *testing.T) {
	core, cfgPath := newTestCore(t)
	pin := "v0.3.0"
	res, err := core.SetBuiltins(BuiltinsParams{BashyVersion: &pin})
	if err != nil {
		t.Fatal(err)
	}
	// A version pin is boot-read, so it must schedule a restart, not be
	// treated as an update-mode-only no-restart change.
	if !res.OK || !res.RestartPending {
		t.Fatalf("unexpected result (want restart-pending): %+v", res)
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fc.BashyVersion != pin {
		t.Fatalf("bashy_version not persisted: %q, want %q", fc.BashyVersion, pin)
	}
	// It must also surface in the SafeView the SPA/CLI read.
	if sv := core.toSafeView(fc); sv.BashyVersion != pin {
		t.Fatalf("bashy_version not in SafeView: %q", sv.BashyVersion)
	}
}
