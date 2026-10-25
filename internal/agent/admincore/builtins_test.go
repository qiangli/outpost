package admincore

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// TestSetBuiltinsRemovedModesAreDead — update_mode / auto_rollback were
// removed from BuiltinsParams. Payloads from older callers (or the old
// SPA) carrying those keys must decode without error and change nothing:
// upgrades always apply and the watchdog always reverts.
func TestSetBuiltinsRemovedModesAreDead(t *testing.T) {
	core, cfgPath := newTestCore(t)
	var p BuiltinsParams
	if err := json.Unmarshal([]byte(`{"update_mode":"never","auto_rollback":true,"mesh":true}`), &p); err != nil {
		t.Fatalf("legacy payload must still decode: %v", err)
	}
	if _, err := core.SetBuiltins(context.Background(), p); err != nil {
		t.Fatalf("SetBuiltins: %v", err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, dead := range []string{"update_mode", "auto_upgrade", "auto_rollback_enabled"} {
		if strings.Contains(string(raw), dead) {
			t.Errorf("config carries removed key %q after SetBuiltins: %s", dead, raw)
		}
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fc.MeshEnabled == nil || !*fc.MeshEnabled {
		t.Fatalf("live field in the same payload must still apply: %+v", fc.MeshEnabled)
	}
}

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
// The route built-ins default ON when their key is absent. So "off" is only
// representable as an explicit false on disk. This test turns each off, then
// performs an UNRELATED SetBuiltins write (the common case: the operator
// toggles something else, or the SPA saves the form) and asserts the opt-out
// is still there afterwards. With a plain bool + omitempty the second write
// drops the key and the routes silently come back on.
//
// It used to assert this over podman/ollama/otel. Those are service units
// now: their desired state is one boolean on a declaration, written through
// Services, so there is no omitempty to erase it — the regression this test
// guards cannot exist for them, and the remaining FileConfig toggles are
// where it still can.
func TestSetBuiltinsOptOutSurvivesLaterWrites(t *testing.T) {
	core, cfgPath := newTestCore(t)
	off := false
	if _, err := core.SetBuiltins(context.Background(), BuiltinsParams{
		Desktop: &off, SSH: &off, SFTP: &off, Mesh: &off,
	}); err != nil {
		t.Fatal(err)
	}
	// An unrelated toggle — must not disturb the opt-outs.
	on := true
	if _, err := core.SetBuiltins(context.Background(), BuiltinsParams{Shell: &on}); err != nil {
		t.Fatal(err)
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fc.DesktopOn() || fc.SSHOn() || fc.SFTPOn() || fc.MeshOn() {
		t.Fatalf("opt-out erased by a later write: desktop=%v ssh=%v sftp=%v mesh=%v",
			fc.DesktopOn(), fc.SSHOn(), fc.SFTPOn(), fc.MeshOn())
	}
	// The SafeView the SPA and `outpost status` render must agree.
	sv := core.toSafeView(fc)
	if sv.DesktopEnabled || sv.SSHEnabled || sv.SFTPEnabled || sv.MeshEnabled {
		t.Fatalf("SafeView disagrees with the config: desktop=%v ssh=%v sftp=%v mesh=%v",
			sv.DesktopEnabled, sv.SSHEnabled, sv.SFTPEnabled, sv.MeshEnabled)
	}
}

// TestSetBuiltinsRejectsRetiredServiceParams — the retired per-service
// toggles must be GONE from the write surface, not quietly accepted and
// dropped. BuiltinsParams is JSON-decoded from the REST body and the MCP
// tool args, so an operator or agent still sending `podman: false` or
// `warm_budget_frac: 0.5` would otherwise get an "ok" for a write that
// never happened.
//
// Go's encoding/json ignores unknown fields, which is exactly the silent
// failure to rule out: the assertion is on the STRUCT, so a field
// reintroduced here fails the build of this test's expectation rather than
// shipping a second copy of a unit's setting.
func TestSetBuiltinsRejectsRetiredServiceParams(t *testing.T) {
	retired := []string{
		"files", "files_allow_write", "files_scope",
		"podman", "sandbox", "ollama", "ollama_pool",
		"warm_serving", "warm_budget_frac",
		"otel", "otel_pool",
		"ycode", "ycode_share", "ycode_share_require_login", "ycode_share_surfaces",
		"lan_inference", "lan_inference_port",
		"shard", "shard_peers", "shard_role",
	}
	// Every field is omitempty, so an empty value marshals to {} — assert on
	// the TYPE's field set instead of on a sample encoding.
	rt := reflect.TypeOf(BuiltinsParams{})
	have := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if name, _, _ := strings.Cut(tag, ","); name != "" {
			have[name] = true
		}
	}
	for _, name := range retired {
		if have[name] {
			t.Errorf("BuiltinsParams still carries %q — it is a service unit's setting now, "+
				"configured through ServiceSettings", name)
		}
	}
	// And the generic pair that replaced them must be there.
	for _, name := range []string{"services", "service_settings"} {
		if !have[name] {
			t.Errorf("BuiltinsParams is missing %q — the generic service write surface", name)
		}
	}
}

// TestSetBuiltinsFreshConfigRouteDefaults — a bare config (no keys set)
// reports the outpost-native routes on at runtime, and cluster off. NOTE:
// the secure opt-in defaults are applied at PAIRING (portal.Exchange writes
// explicit false), not at this runtime layer — so a bare LoadFile still
// reflects the legacy default-on.
func TestSetBuiltinsFreshConfigRouteDefaults(t *testing.T) {
	core, cfgPath := newTestCore(t)
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !fc.ShellOn() || !fc.DesktopOn() || !fc.ClipboardOn() || !fc.SSHOn() {
		t.Fatalf("fresh config: shell=%v desktop=%v clipboard=%v ssh=%v, want all on",
			fc.ShellOn(), fc.DesktopOn(), fc.ClipboardOn(), fc.SSHOn())
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
