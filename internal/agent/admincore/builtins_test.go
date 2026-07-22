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

// TestSetBuiltinsOptOutSurvivesLaterWrites — the end-to-end form of the
// opt-out trap, exercised through the real write path (SetBuiltins →
// SaveFile → LoadFile) rather than the struct alone.
//
// podman/ollama/otel default ON when their key is absent. So "off" is
// only representable as an explicit false on disk. This test turns each
// off, then performs an UNRELATED SetBuiltins write (the common case:
// the operator toggles something else, or the SPA saves the form) and
// asserts the opt-out is still there afterwards. With a plain bool +
// omitempty the second write drops the key and the services silently
// come back on.
func TestSetBuiltinsOptOutSurvivesLaterWrites(t *testing.T) {
	core, cfgPath := newTestCore(t)
	off := false
	if _, err := core.SetBuiltins(BuiltinsParams{
		Podman: &off, Ollama: &off, Otel: &off,
	}); err != nil {
		t.Fatal(err)
	}
	// An unrelated toggle — must not disturb the o3 opt-outs.
	on := true
	if _, err := core.SetBuiltins(BuiltinsParams{Shell: &on}); err != nil {
		t.Fatal(err)
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fc.PodmanOn() || fc.OllamaOn() || fc.OtelOn() {
		t.Fatalf("opt-out erased by a later write: podman=%v ollama=%v otel=%v",
			fc.PodmanOn(), fc.OllamaOn(), fc.OtelOn())
	}
	// The SafeView the SPA and `outpost status` render must agree.
	sv := core.toSafeView(fc)
	if sv.Podman.Enabled || sv.Ollama.Enabled || sv.OtelEnabled {
		t.Fatalf("SafeView disagrees with the config: podman=%v ollama=%v otel=%v",
			sv.Podman.Enabled, sv.Ollama.Enabled, sv.OtelEnabled)
	}
}

// TestSetBuiltinsFreshConfigDefaultsO3On — a config the operator has
// never touched reports podman/ollama/otel on, and cluster off.
func TestSetBuiltinsFreshConfigDefaultsO3On(t *testing.T) {
	core, cfgPath := newTestCore(t)
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !fc.PodmanOn() || !fc.OllamaOn() || !fc.OtelOn() {
		t.Fatalf("fresh config: podman=%v ollama=%v otel=%v, want all on",
			fc.PodmanOn(), fc.OllamaOn(), fc.OtelOn())
	}
	if fc.ClusterOn() {
		t.Fatalf("fresh config: cluster on, want opt-in")
	}
	_ = core
}
