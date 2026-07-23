package conf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestFileConfigRoundTrip — write a full FileConfig (apps + toggles) to
// disk, read it back, and assert every field survives.
func TestFileConfigRoundTrip(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "agent.json")
	on := true
	off := false
	in := &FileConfig{
		AgentName:        "host-1",
		ServerAddr:       "edge.example.com",
		ServerPort:       443,
		Protocol:         "wss",
		Token:            "tok",
		RemotePort:       7100,
		AuthURL:          "",
		ShellEnabled:     &on,
		DesktopEnabled:   &off,
		ClipboardEnabled: &on,
		SSHEnabled:       &off,
		Apps: []AppConfig{
			{Name: "ycode", Scheme: "http", Host: "127.0.0.1", Port: 8765, Enabled: true},
			{Name: "jupyter", Scheme: "http", Host: "127.0.0.1", Port: 8888, Enabled: false, Icon: "/x.png"},
			{Name: "podman", Scheme: "unix", Socket: "/run/user/1000/podman/podman.sock", Enabled: true, Role: "admin"},
		},
	}
	if err := SaveFile(tmp, in); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	out, err := LoadFile(tmp)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if out.AgentName != in.AgentName || out.Token != in.Token || out.RemotePort != in.RemotePort {
		t.Errorf("scalar fields drifted: %+v vs %+v", out, in)
	}
	if !out.ShellOn() || out.DesktopOn() || !out.ClipboardOn() || out.SSHOn() {
		t.Errorf("toggle round-trip: shell=%v desktop=%v clipboard=%v ssh=%v",
			out.ShellOn(), out.DesktopOn(), out.ClipboardOn(), out.SSHOn())
	}
	if len(out.Apps) != 3 || out.Apps[0].Port != 8765 || out.Apps[1].Enabled {
		t.Errorf("apps round-trip: %+v", out.Apps)
	}
	// Socket-backed entry must round-trip Scheme + Socket and keep
	// Host/Port at their zero values (JSON omitempty leaves them out).
	sockApp := out.Apps[2]
	if sockApp.Scheme != "unix" || sockApp.Socket != "/run/user/1000/podman/podman.sock" {
		t.Errorf("socket-app round-trip: %+v", sockApp)
	}
	if sockApp.Host != "" || sockApp.Port != 0 {
		t.Errorf("socket-app should not carry host/port: %+v", sockApp)
	}
	if !sockApp.IsSocket() {
		t.Error("IsSocket() should return true for scheme=unix")
	}
}

// TestFileConfigLegacyDefaults — a config written before the admin UI
// shipped has no "shell_enabled"/"desktop_enabled" keys. Those nil
// pointers MUST mean "default on" so existing installs don't silently
// lose features after upgrading.
func TestFileConfigLegacyDefaults(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "agent.json")
	legacy := []byte(`{"agent_name":"old","token":"t","server_addr":"a","server_port":7000,"remote_port":7100}`)
	if err := os.WriteFile(tmp, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := LoadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	// A legacy config that never set these keys keeps the runtime default
	// of ON — the opt-in posture applies only to NEW pairings (written at
	// Exchange), so existing hosts are never disabled by an upgrade.
	if !out.ShellOn() || !out.DesktopOn() || !out.ClipboardOn() || !out.SSHOn() {
		t.Errorf("legacy config should stay default-on: shell=%v desktop=%v clipboard=%v ssh=%v",
			out.ShellOn(), out.DesktopOn(), out.ClipboardOn(), out.SSHOn())
	}
	if out.Apps != nil {
		t.Errorf("legacy config Apps should be nil (so MATRIX_APPS env still wins): got %+v", out.Apps)
	}
}

// TestNilFileConfigAccessors — the convenience accessors must not panic
// on a nil receiver. Callers occasionally test ShellOn() before loading
// the file.
func TestNilFileConfigAccessors(t *testing.T) {
	var fc *FileConfig
	if !fc.ShellOn() || !fc.DesktopOn() || !fc.ClipboardOn() || !fc.SSHOn() {
		t.Error("nil FileConfig should default-on")
	}
}

func TestSupervisedPrograms(t *testing.T) {
	off := false
	on := true
	fc := &FileConfig{Supervised: []SupervisedProgram{
		{Name: "qa-poller", Path: "/usr/local/bin/bashy", Args: []string{"qa-poller.sh"}, Dir: "/qa"},
		{Path: "/opt/bin/helper"},                       // name defaults to base
		{Name: "disabled", Path: "/x/y", Enabled: &off}, // explicitly off
		{Name: "explicit-on", Path: "/z", Enabled: &on},
		{Name: "broken"},                  // no path — skipped
		{Name: "blank-path", Path: "   "}, // whitespace — skipped
	}}

	got, skipped := fc.SupervisedPrograms()
	var names []string
	for _, sp := range got {
		names = append(names, sp.Name)
	}
	want := []string{"qa-poller", "helper", "explicit-on"}
	if !slices.Equal(names, want) {
		t.Errorf("enabled programs = %v, want %v", names, want)
	}
	if len(skipped) != 2 {
		t.Errorf("expected 2 skipped entries, got %v", skipped)
	}
	// A malformed entry must be reported, not silently dropped — otherwise
	// a typo in agent.json looks identical to a job with nothing to do.
	for _, s := range skipped {
		if !strings.Contains(s, "empty path") {
			t.Errorf("skip reason should explain itself: %q", s)
		}
	}
}

// A missing "enabled" field means enabled: an entry someone added to the
// config is meant to run.
func TestSupervisedProgramOnDefaultsTrue(t *testing.T) {
	if !(SupervisedProgram{Path: "/x"}).On() {
		t.Error("missing enabled field should default to on")
	}
	off := false
	if (SupervisedProgram{Path: "/x", Enabled: &off}).On() {
		t.Error("enabled=false should be off")
	}
}

// ClusterOn is OPT-IN (default off): joining hands a remote control plane
// the right to schedule privileged work here, so it must be explicit. An
// empty ClusterConfig{} (what reattach/Exchange create to stash creds)
// must NOT flip it on — the *bool nil stays off. Only an explicit true
// joins.
func TestClusterOffByDefault(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name string
		fc   *FileConfig
		want bool
	}{
		{"nil config", nil, false},
		{"nil cluster block", &FileConfig{}, false},
		{"empty cluster block (creds stashed, not enabled) → off", &FileConfig{Cluster: &ClusterConfig{}}, false},
		{"explicit opt-in → on", &FileConfig{Cluster: &ClusterConfig{Enabled: &on}}, true},
		{"explicit off", &FileConfig{Cluster: &ClusterConfig{Enabled: &off}}, false},
	}
	for _, tc := range cases {
		if got := tc.fc.ClusterOn(); got != tc.want {
			t.Errorf("%s: ClusterOn()=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSupervisedProgramsNilConfig(t *testing.T) {
	var fc *FileConfig
	got, skipped := fc.SupervisedPrograms()
	if got != nil || skipped != nil {
		t.Errorf("nil config should yield nothing, got %v / %v", got, skipped)
	}
}

// TestSaveFileAtomic — a half-written .tmp file should not clobber a
// good config when the save fails. SaveFile only renames on success.
func TestSaveFileAtomic(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "agent.json")
	in := &FileConfig{AgentName: "x", Token: "t"}
	if err := SaveFile(tmp, in); err != nil {
		t.Fatal(err)
	}
	// Confirm the .tmp file was cleaned up after rename.
	if _, err := os.Stat(tmp + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp lingered: err = %v", err)
	}
	raw, _ := os.ReadFile(tmp)
	var v struct {
		AgentName string `json:"agent_name"`
	}
	_ = json.Unmarshal(raw, &v)
	if v.AgentName != "x" {
		t.Errorf("saved JSON = %s", raw)
	}
}

// TestOllamaPoolOn — pool participation is gated by OllamaOn, but
// once that's true the default with no explicit toggle is "on" so the
// user opts into pooling implicitly by enabling Ollama.
func TestOllamaPoolOn(t *testing.T) {
	off := false
	on := true
	for _, tt := range []struct {
		name string
		fc   *FileConfig
		want bool
	}{
		{"nil-fc", nil, false},
		{"ollama-off", &FileConfig{OllamaEnabled: bp(false)}, false},
		{"ollama-on-pool-unset", &FileConfig{OllamaEnabled: bp(true)}, true},
		{"ollama-on-pool-explicit-on", &FileConfig{OllamaEnabled: bp(true), OllamaPoolEnabled: &on}, true},
		{"ollama-on-pool-explicit-off", &FileConfig{OllamaEnabled: bp(true), OllamaPoolEnabled: &off}, false},
		{"ollama-off-pool-on-ignored", &FileConfig{OllamaEnabled: bp(false), OllamaPoolEnabled: &on}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fc.OllamaPoolOn(); got != tt.want {
				t.Errorf("OllamaPoolOn()=%v, want %v", got, tt.want)
			}
		})
	}
}

// TestLANInferenceOn — same-LAN direct inference is gated by OllamaOn
// AND, unlike the pool, defaults OFF (nil ⇒ off) because it is a LAN-trust
// endpoint that must be an explicit opt-in.
func TestLANInferenceOn(t *testing.T) {
	off := false
	on := true
	for _, tt := range []struct {
		name string
		fc   *FileConfig
		want bool
	}{
		{"nil-fc", nil, false},
		{"ollama-off", &FileConfig{OllamaEnabled: bp(false)}, false},
		{"ollama-on-lan-unset-default-off", &FileConfig{OllamaEnabled: bp(true)}, false},
		{"ollama-on-lan-explicit-on", &FileConfig{OllamaEnabled: bp(true), LANInferenceEnabled: &on}, true},
		{"ollama-on-lan-explicit-off", &FileConfig{OllamaEnabled: bp(true), LANInferenceEnabled: &off}, false},
		{"ollama-off-lan-on-ignored", &FileConfig{OllamaEnabled: bp(false), LANInferenceEnabled: &on}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fc.LANInferenceOn(); got != tt.want {
				t.Errorf("LANInferenceOn()=%v, want %v", got, tt.want)
			}
		})
	}
}

// TestLANInferencePortOrDefault — 11435 default, configured value wins.
func TestLANInferencePortOrDefault(t *testing.T) {
	for _, tt := range []struct {
		name string
		fc   *FileConfig
		want int
	}{
		{"nil-fc", nil, 11435},
		{"unset", &FileConfig{}, 11435},
		{"zero", &FileConfig{LANInferencePort: 0}, 11435},
		{"explicit", &FileConfig{LANInferencePort: 12000}, 12000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fc.LANInferencePortOrDefault(); got != tt.want {
				t.Errorf("LANInferencePortOrDefault()=%d, want %d", got, tt.want)
			}
		})
	}
}

// TestEnsureAppSSOSecrets — boot-time safety net. Apps with
// TrustCloudIdentity:true but no SSOSecret get a freshly minted
// 32-byte hex secret; persisted to disk; other apps untouched.
func TestEnsureAppSSOSecrets(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "agent.json")
	fc := &FileConfig{
		Apps: []AppConfig{
			{Name: "trusted-no-secret", TrustCloudIdentity: true},
			{Name: "trusted-has-secret", TrustCloudIdentity: true, SSOSecret: "existing"},
			{Name: "untrusted", TrustCloudIdentity: false},
		},
	}
	if err := SaveFile(tmp, fc); err != nil {
		t.Fatal(err)
	}

	minted, err := EnsureAppSSOSecrets(tmp, fc)
	if err != nil {
		t.Fatalf("EnsureAppSSOSecrets: %v", err)
	}
	if len(minted) != 1 || minted[0] != "trusted-no-secret" {
		t.Errorf("minted = %v, want [trusted-no-secret]", minted)
	}
	if got := fc.Apps[0].SSOSecret; len(got) != 64 {
		t.Errorf("trusted-no-secret SSOSecret len = %d, want 64 (32 hex bytes)", len(got))
	}
	if fc.Apps[1].SSOSecret != "existing" {
		t.Errorf("trusted-has-secret SSOSecret = %q, want unchanged", fc.Apps[1].SSOSecret)
	}
	if fc.Apps[2].SSOSecret != "" {
		t.Errorf("untrusted SSOSecret = %q, want empty (TrustCloudIdentity off)", fc.Apps[2].SSOSecret)
	}

	// Persisted: round-trip from disk should see the new secret.
	loaded, err := LoadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Apps[0].SSOSecret != fc.Apps[0].SSOSecret {
		t.Errorf("SSOSecret not persisted; disk=%q in-mem=%q",
			loaded.Apps[0].SSOSecret, fc.Apps[0].SSOSecret)
	}

	// Second call is a no-op (idempotent).
	minted2, err := EnsureAppSSOSecrets(tmp, fc)
	if err != nil {
		t.Fatal(err)
	}
	if len(minted2) != 0 {
		t.Errorf("second call minted = %v, want []", minted2)
	}
}

// TestNormalizeClusterMode locks in the back-compat aliases — the
// persisted "vkpodman" wire value and an empty Mode both resolve to
// vk-podman, while the canonical modes round-trip unchanged.
func TestNormalizeClusterMode(t *testing.T) {
	cases := map[string]string{
		"":           ClusterModeVKPodman,
		"vkpodman":   ClusterModeVKPodman,
		"VKPodman":   ClusterModeVKPodman,
		" vkpodman ": ClusterModeVKPodman,
		"vk-podman":  ClusterModeVKPodman,
		"agent":      ClusterModeAgentMode,
		"AGENT":      ClusterModeAgentMode,
		"vk-native":  ClusterModeVKNative,
		"VK-Native":  ClusterModeVKNative,
		"vk-ollama":  ClusterModeVKOllama,
		"VK-Ollama":  ClusterModeVKOllama,
	}
	for in, want := range cases {
		if got := NormalizeClusterMode(in); got != want {
			t.Errorf("NormalizeClusterMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestClusterModeHelpers verifies the predicate helpers off the
// normalized mode, including the load-bearing rule that "vkpodman" and
// "" select neither agent nor native-process (i.e. the libpod vk-podman
// backend).
func TestClusterModeHelpers(t *testing.T) {
	for _, mode := range []string{"", "vkpodman", "vk-podman"} {
		c := &ClusterConfig{Mode: mode}
		if c.ClusterModeAgent() {
			t.Errorf("Mode=%q: ClusterModeAgent() = true, want false", mode)
		}
		if c.ClusterModeVKNative() {
			t.Errorf("Mode=%q: ClusterModeVKNative() = true, want false", mode)
		}
		if c.ClusterModeVKOllama() {
			t.Errorf("Mode=%q: ClusterModeVKOllama() = true, want false", mode)
		}
		if c.ClusterModeNativeProcess() {
			t.Errorf("Mode=%q: ClusterModeNativeProcess() = true, want false", mode)
		}
		if c.ClusterMode() != ClusterModeVKPodman {
			t.Errorf("Mode=%q: ClusterMode() = %q, want vk-podman", mode, c.ClusterMode())
		}
	}
	if c := (&ClusterConfig{Mode: "agent"}); !c.ClusterModeAgent() || c.ClusterModeVKNative() || c.ClusterModeVKOllama() || c.ClusterModeNativeProcess() {
		t.Errorf("Mode=agent: helpers wrong (agent=%v native=%v ollama=%v nativeProcess=%v)", c.ClusterModeAgent(), c.ClusterModeVKNative(), c.ClusterModeVKOllama(), c.ClusterModeNativeProcess())
	}
	if c := (&ClusterConfig{Mode: "vk-native"}); !c.ClusterModeVKNative() || !c.ClusterModeNativeProcess() || c.ClusterModeAgent() || c.ClusterModeVKOllama() {
		t.Errorf("Mode=vk-native: helpers wrong (native=%v nativeProcess=%v agent=%v ollama=%v)", c.ClusterModeVKNative(), c.ClusterModeNativeProcess(), c.ClusterModeAgent(), c.ClusterModeVKOllama())
	}
	if c := (&ClusterConfig{Mode: "vk-ollama"}); !c.ClusterModeVKOllama() || !c.ClusterModeNativeProcess() || c.ClusterModeAgent() || c.ClusterModeVKNative() {
		t.Errorf("Mode=vk-ollama: helpers wrong (agent=%v native=%v ollama=%v nativeProcess=%v)", c.ClusterModeAgent(), c.ClusterModeVKNative(), c.ClusterModeVKOllama(), c.ClusterModeNativeProcess())
	}
	// nil receiver normalizes like an empty Mode.
	var nilc *ClusterConfig
	if nilc.ClusterMode() != ClusterModeVKPodman || nilc.ClusterModeAgent() || nilc.ClusterModeVKNative() || nilc.ClusterModeVKOllama() || nilc.ClusterModeNativeProcess() {
		t.Errorf("nil receiver: helpers wrong")
	}
}

// TestO3BuiltinsDefaultOn — with none of the keys set, the o3 built-ins
// (podman / ollama / otel) report ON at runtime: the opt-in posture is
// applied to NEW pairings at Exchange, not via the runtime default, so
// existing/legacy configs are unchanged. Cluster stays OFF: joining hands
// a remote control plane the right to schedule work here — a choice, not a
// default. See the PodmanOn / ClusterOn doc comments.
func TestO3BuiltinsDefaultOn(t *testing.T) {
	fc := &FileConfig{}
	if !fc.PodmanOn() || !fc.OllamaOn() || !fc.OtelOn() {
		t.Errorf("unset o3 keys: podman=%v ollama=%v otel=%v, want all true",
			fc.PodmanOn(), fc.OllamaOn(), fc.OtelOn())
	}
	if fc.ClusterOn() {
		t.Errorf("ClusterOn() = true for an unset config; cluster must stay opt-in")
	}
	// Sandbox is NOT part of the flip — it widens who may run containers.
	if fc.SandboxOn() {
		t.Errorf("SandboxOn() = true for an unset config; sandbox must stay opt-in")
	}
	// A nil config still reports off everywhere (no panics, no defaults
	// conjured out of a missing config).
	var nilfc *FileConfig
	if nilfc.PodmanOn() || nilfc.OllamaOn() || nilfc.OtelOn() || nilfc.ClusterOn() {
		t.Errorf("nil config reported an o3 built-in as on")
	}
}

// TestO3ExplicitFalseSurvivesRoundTrip — THE regression test for the
// opt-out-that-erases-itself trap.
//
// The o3 enable keys carry `omitempty`. Were they plain `bool`, an
// operator's explicit false would marshal to ABSENT — byte-identical to
// never-set — so the very next load would apply the default-ON rule and
// silently restart the service they just turned off. Pointer-bool is what
// keeps unset / true / false distinguishable, and this test pins that:
// the JSON must literally contain `false`, and the reloaded config must
// still report off. Repeat the save/load cycle to catch a decay that only
// shows up on the second write.
func TestO3ExplicitFalseSurvivesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	off := false
	if err := SaveFile(path, &FileConfig{
		AgentName:     "opted-out",
		PodmanEnabled: &off,
		OllamaEnabled: &off,
		OtelEnabled:   &off,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	for cycle := 0; cycle < 3; cycle++ {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("cycle %d: read: %v", cycle, err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatalf("cycle %d: unmarshal: %v", cycle, err)
		}
		for _, k := range []string{"podman_enabled", "ollama_enabled", "otel_enabled"} {
			v, ok := keys[k]
			if !ok {
				t.Fatalf("cycle %d: %q absent from the saved JSON — an explicit "+
					"opt-out was erased by omitempty; the default-ON rule will "+
					"resurrect the service on next load. Raw: %s", cycle, k, raw)
			}
			if string(v) != "false" {
				t.Fatalf("cycle %d: %q = %s, want false", cycle, k, v)
			}
		}

		got, err := LoadFile(path)
		if err != nil {
			t.Fatalf("cycle %d: load: %v", cycle, err)
		}
		if got.PodmanOn() || got.OllamaOn() || got.OtelOn() {
			t.Fatalf("cycle %d: opt-out lost: podman=%v ollama=%v otel=%v",
				cycle, got.PodmanOn(), got.OllamaOn(), got.OtelOn())
		}
		// Save the loaded config straight back — this is what every
		// admin-UI/CLI write does (load, mutate one field, save). It is
		// the step where a plain bool would drop the false.
		if err := SaveFile(path, got); err != nil {
			t.Fatalf("cycle %d: resave: %v", cycle, err)
		}
	}
}

// TestO3ExplicitTrueSurvivesRoundTrip — the mirror case. An explicit
// true is redundant with the default today, but it must persist as a
// real `true` so a future default change can't silently flip it either.
func TestO3ExplicitTrueSurvivesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	on := true
	if err := SaveFile(path, &FileConfig{
		AgentName:     "opted-in",
		PodmanEnabled: &on,
		OllamaEnabled: &on,
		OtelEnabled:   &on,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for name, p := range map[string]*bool{
		"podman_enabled": got.PodmanEnabled,
		"ollama_enabled": got.OllamaEnabled,
		"otel_enabled":   got.OtelEnabled,
	} {
		if p == nil {
			t.Errorf("%s: nil after round trip, want an explicit true", name)
		} else if !*p {
			t.Errorf("%s: false after round trip, want true", name)
		}
	}
}
