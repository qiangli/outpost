package admincore

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func TestSetBuiltinsPersistsBashyVersionPin(t *testing.T) {
	core, cfgPath := newTestCore(t)
	pin := "v0.3.0"
	res, err := core.SetBuiltins(context.Background(), BuiltinsParams{BashyVersion: &pin})
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
	if _, err := core.SetBuiltins(context.Background(), BuiltinsParams{
		Podman: &off, Ollama: &off, Otel: &off,
	}); err != nil {
		t.Fatal(err)
	}
	// An unrelated toggle — must not disturb the o3 opt-outs.
	on := true
	if _, err := core.SetBuiltins(context.Background(), BuiltinsParams{Shell: &on}); err != nil {
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

// TestSetBuiltinsFreshConfigDefaultsO3On — a bare config (no keys set)
// reports podman/ollama/otel on at runtime, and cluster off. NOTE: the
// secure opt-in defaults for podman/shell/etc. are applied at PAIRING
// (portal.NewFromExchange writes explicit false), not at this runtime
// layer — so a bare LoadFile still reflects the legacy default-on.
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

func TestSetBuiltinsConcurrentClusterRuntimes(t *testing.T) {
	core, cfgPath := newTestCore(t)
	on := true
	virtual := []string{"vk-native", "vk-podman", "vk-ollama"}
	if _, err := core.SetBuiltins(context.Background(), BuiltinsParams{
		Cluster:        &on,
		ClusterAgent:   &on,
		ClusterVirtual: virtual,
	}); err != nil {
		t.Fatal(err)
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !fc.Cluster.HasAgentRuntime() || !reflect.DeepEqual(fc.Cluster.VirtualRuntimes(), virtual) {
		t.Fatalf("runtimes = %+v", fc.Cluster.Runtimes)
	}
}

// A client that writes back what a redacted read gave it must not wipe the
// stored trusted-identity keys.
//
// This used to need care: bashy_services was a whole array the caller
// replaced, each row carrying an sso_secret that reads blanked, so a
// round-trip could clear the key the cooperating upstream had learned. The
// shape is gone — keys live in outpost's own service_sso map, which no
// request field can reach — so the guarantee is now structural. Assert that
// rather than the old merge behaviour.
func TestServiceWritesCannotClearAStoredSSOKey(t *testing.T) {
	core, cfgPath := newTestCore(t)
	const secret = "synthetic-service-sso-secret-dddddddddddddd"
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	fc.ServiceSSO = map[string]string{"loom": secret}
	if err := conf.SaveFile(cfgPath, fc); err != nil {
		t.Fatal(err)
	}

	ops := newStubOps("loom")
	core.SetServices(ops)
	if _, err := core.SetBuiltins(context.Background(), BuiltinsParams{
		Services:        map[string]bool{"loom": true},
		ServiceSettings: map[string]map[string]string{"loom": {"ports.0.port": "31881"}},
	}); err != nil {
		t.Fatal(err)
	}

	after, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.ServiceSSO["loom"] != secret {
		t.Fatalf("a service write disturbed the stored key: %+v", after.ServiceSSO)
	}

	// And a read never carries it — only its presence.
	view, err := core.SafeView()
	if err != nil {
		t.Fatal(err)
	}
	red := view.Redacted()
	blob, err := json.Marshal(red)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), secret) {
		t.Fatalf("a redacted read carried the key: %s", blob)
	}
	if len(red.ServiceSecrets) != 1 || red.ServiceSecrets[0].Name != "loom" || !red.ServiceSecrets[0].HasSSOSecret {
		t.Fatalf("service_secrets = %+v, want one presence row for loom", red.ServiceSecrets)
	}
}
