package conf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestFileConfigRoundTrip — write a full FileConfig (apps + toggles) to
// disk, read it back, and assert every field survives.
func bp(b bool) *bool { return &b }

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
		ServiceSSO: map[string]string{"trusted-service": "service-secret"},
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
	if out.ServiceSSO["trusted-service"] != "service-secret" {
		t.Errorf("service SSO keys did not round-trip: %+v", out.ServiceSSO)
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

func TestClusterRuntimes(t *testing.T) {
	c := &ClusterConfig{Runtimes: ClusterRuntimes{
		Agent:   true,
		Virtual: []string{ClusterRuntimeVKNative, ClusterRuntimeVKPodman, ClusterRuntimeVKOllama},
	}}
	if !c.HasAgentRuntime() {
		t.Fatal("agent runtime not enabled")
	}
	got := c.VirtualRuntimes()
	if len(got) != 3 {
		t.Fatalf("VirtualRuntimes = %v", got)
	}
	got[0] = "changed"
	if c.Runtimes.Virtual[0] != ClusterRuntimeVKNative {
		t.Fatal("VirtualRuntimes returned the config's backing slice")
	}
	for _, mode := range c.Runtimes.Virtual {
		if !ValidVirtualRuntime(mode) {
			t.Errorf("ValidVirtualRuntime(%q) = false", mode)
		}
	}
	if ValidVirtualRuntime(ClusterRuntimeAgent) || ValidVirtualRuntime("unknown") {
		t.Fatal("non-virtual runtime accepted")
	}
	if err := c.ValidateRuntimes(); err != nil {
		t.Fatal(err)
	}
	if err := (&ClusterConfig{}).ValidateRuntimes(); err == nil {
		t.Fatal("empty runtime set accepted")
	}
	if err := (&ClusterConfig{Runtimes: ClusterRuntimes{
		Virtual: []string{ClusterRuntimeVKNative, ClusterRuntimeVKNative},
	}}).ValidateRuntimes(); err == nil {
		t.Fatal("duplicate runtime accepted")
	}
}

// TestJoinTarget_PeerThenLeaveFallsBackToCloudbox pins the config-level
// invariant behind `outpost cluster leave` on a peer-joined worker: while a
// join endpoint is configured JoinTarget routes the runtime at the PEER plane,
// and once leave clears the peer membership fields it falls cleanly back to the
// cloudbox pairing — so a leave leaves no half-attached state and a rejoin is
// clean.
func TestJoinTarget_PeerThenLeaveFallsBackToCloudbox(t *testing.T) {
	fc := &FileConfig{
		ServerAddr: "ai.dhnt.io",
		ServerPort: 7000,
		Token:      "cloudbox-tunnel-token",
		Cluster: &ClusterConfig{
			JoinEndpoint: "10.0.0.5:7100",
			JoinToken:    "peer-tunnel-token",
		},
	}
	// Peer plane while joined.
	if !fc.Cluster.JoinsPeerPlane() {
		t.Fatal("configured join endpoint must report as a peer plane")
	}
	h, p, tok, user := fc.JoinTarget()
	if h != "10.0.0.5" || p != 7100 || tok != "peer-tunnel-token" || user != ControlPlanePublisherUser {
		t.Errorf("peer JoinTarget = %q/%d/%q/%q", h, p, tok, user)
	}

	// Leave clears the peer membership fields (what disableClusterMembership does).
	fc.Cluster.JoinEndpoint = ""
	fc.Cluster.JoinToken = ""
	fc.Cluster.NodeToken = ""
	fc.Cluster.STCPSecret = ""

	if fc.Cluster.JoinsPeerPlane() {
		t.Error("still a peer plane after clearing the join endpoint")
	}
	h, p, tok, user = fc.JoinTarget()
	if h != "ai.dhnt.io" || p != 7000 || tok != "cloudbox-tunnel-token" || user != "cloudbox" {
		t.Errorf("post-leave JoinTarget did not fall back to cloudbox: %q/%d/%q/%q", h, p, tok, user)
	}
}

// A config with no toggle keys at all must report the outpost-native route
// built-ins ON at runtime: the opt-in posture is applied to NEW pairings at
// Exchange, not via the runtime default, so existing/legacy configs are
// unchanged by an upgrade. Cluster stays OFF: joining hands a remote control
// plane the right to schedule work here — a choice, not a default.
//
// The podman / ollama / otel proxies used to be asserted here too. They are
// service units now, so their default posture is their declaration's
// `enabled` bit and there is nothing in FileConfig to assert about them.
func TestRouteBuiltinsDefaultOn(t *testing.T) {
	fc := &FileConfig{}
	if !fc.ShellOn() || !fc.DesktopOn() || !fc.ClipboardOn() || !fc.SSHOn() || !fc.SFTPOn() {
		t.Errorf("unset route keys: shell=%v desktop=%v clipboard=%v ssh=%v sftp=%v, want all true",
			fc.ShellOn(), fc.DesktopOn(), fc.ClipboardOn(), fc.SSHOn(), fc.SFTPOn())
	}
	if !fc.MeshOn() {
		t.Errorf("MeshOn() = false for an unset config; the mesh defaults on for a paired host")
	}
	if fc.ClusterOn() {
		t.Errorf("ClusterOn() = true for an unset config; cluster must stay opt-in")
	}
	var nilfc *FileConfig
	if nilfc.ClusterOn() {
		t.Errorf("nil config reported the cluster join as on")
	}
}

// TestExplicitFalseSurvivesRoundTrip — THE regression test for the
// opt-out-that-erases-itself trap.
//
// The enable keys carry `omitempty`. Were they plain `bool`, an operator's
// explicit false would marshal to ABSENT — byte-identical to never-set — so
// the very next load would apply the default-ON rule and silently restore the
// route they just turned off. Pointer-bool is what keeps unset / true / false
// distinguishable, and this test pins that: the JSON must literally contain
// `false`, and the reloaded config must still report off. Repeat the
// save/load cycle to catch a decay that only shows up on the second write.
func TestExplicitFalseSurvivesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	off := false
	if err := SaveFile(path, &FileConfig{
		AgentName:    "opted-out",
		ShellEnabled: &off,
		SSHEnabled:   &off,
		SFTPEnabled:  &off,
		MeshEnabled:  &off,
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
		for _, k := range []string{"shell_enabled", "ssh_enabled", "sftp_enabled", "mesh_enabled"} {
			v, ok := keys[k]
			if !ok {
				t.Fatalf("cycle %d: %q absent from the saved JSON — an explicit "+
					"opt-out was erased by omitempty; the default-ON rule will "+
					"resurrect the route on next load. Raw: %s", cycle, k, raw)
			}
			if string(v) != "false" {
				t.Fatalf("cycle %d: %q = %s, want false", cycle, k, v)
			}
		}

		got, err := LoadFile(path)
		if err != nil {
			t.Fatalf("cycle %d: load: %v", cycle, err)
		}
		if got.ShellOn() || got.SSHOn() || got.SFTPOn() || got.MeshOn() {
			t.Fatalf("cycle %d: opt-out lost: shell=%v ssh=%v sftp=%v mesh=%v",
				cycle, got.ShellOn(), got.SSHOn(), got.SFTPOn(), got.MeshOn())
		}
		// Save the loaded config straight back — this is what every
		// admin-UI/CLI write does (load, mutate one field, save). It is
		// the step where a plain bool would drop the false.
		if err := SaveFile(path, got); err != nil {
			t.Fatalf("cycle %d: resave: %v", cycle, err)
		}
	}
}

// TestExplicitTrueSurvivesRoundTrip — the mirror case. An explicit true is
// redundant with the default today, but it must persist as a real `true` so a
// future default change can't silently flip it either.
func TestExplicitTrueSurvivesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	on := true
	if err := SaveFile(path, &FileConfig{
		AgentName:    "opted-in",
		ShellEnabled: &on,
		SSHEnabled:   &on,
		MeshEnabled:  &on,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for name, p := range map[string]*bool{
		"shell_enabled": got.ShellEnabled,
		"ssh_enabled":   got.SSHEnabled,
		"mesh_enabled":  got.MeshEnabled,
	} {
		if p == nil {
			t.Errorf("%s: nil after round trip, want an explicit true", name)
		} else if !*p {
			t.Errorf("%s: false after round trip, want true", name)
		}
	}
}

// EnsureServiceSSOSecret is the service-unit counterpart of
// EnsureAppSSOSecrets: a unit declaring expose.app.trust_cloud_identity says
// it WANTS an HMAC key (a declaration never holds an inline secret), and this
// mints one keyed by app route, persists it, and returns the same value every
// time after. A rotation on every boot would silently invalidate every
// signature the cooperating upstream had learned.
func TestEnsureServiceSSOSecret(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "agent.json")
	fc := &FileConfig{AgentName: "dragon"}

	first, err := EnsureServiceSSOSecret(tmp, fc, "console")
	if err != nil {
		t.Fatalf("EnsureServiceSSOSecret: %v", err)
	}
	if len(first) != 64 {
		t.Fatalf("minted key len = %d, want 64 (32 hex bytes)", len(first))
	}
	again, err := EnsureServiceSSOSecret(tmp, fc, "console")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if again != first {
		t.Fatalf("key rotated on a second call: %q then %q", first, again)
	}
	other, err := EnsureServiceSSOSecret(tmp, fc, "rooms")
	if err != nil {
		t.Fatalf("second app: %v", err)
	}
	if other == first {
		t.Fatal("two app routes share one key; a compromise of either would forge for both")
	}

	loaded, err := LoadFile(tmp)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if loaded.ServiceSSO["console"] != first || loaded.ServiceSSO["rooms"] != other {
		t.Fatalf("keys were not persisted: %+v", loaded.ServiceSSO)
	}

	if _, err := EnsureServiceSSOSecret(tmp, fc, "   "); err == nil {
		t.Fatal("an empty app name was accepted")
	}
	if _, err := EnsureServiceSSOSecret(tmp, nil, "x"); err == nil {
		t.Fatal("a nil config was accepted")
	}
}

// Every long-lived service — the external ones (the Zot registry, the
// SeaweedFS S3 gateway, the Kopia backup repo, the Gitea CI runner, the
// Headlamp forward) AND the ones outpost hosts in-process (the File Browser,
// the podman and sandbox container mounts, the Ollama proxy and its pool, the
// warm and shard planes, the same-LAN inference listener, the observability
// and ycode surfaces) — is a service UNIT now, declared in fleet's registry
// rings and controlled by its `enabled` bit plus its `settings`. The
// agent.json keys that used to configure them are retired outright, with no
// compatibility reader (operator clarification 2026-10-10): a stale config
// carrying them must parse cleanly and forget them, so an upgraded host
// cannot keep a second, invisible copy of a port, a budget or a token that
// nothing honours.
func TestRetiredExternalServiceKeysAreNotRead(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "agent.json")
	stale := []byte(`{
	  "agent_name": "host-1",
	  "zot_enabled": true, "zot_port": 5000,
	  "seaweedfs_enabled": true, "seaweedfs_port": 8333,
	  "kopia_enabled": true, "kopia_port": 51515,
	  "actrunner_enabled": true, "actrunner_instance": "https://forge.example.com",
	  "actrunner_token": "tok", "actrunner_labels": "host:host",
	  "actrunner_sandbox": true, "actrunner_sandbox_image": "img",
	  "actrunner_docker_host": "unix:///x.sock",
	  "headlamp_enabled": true, "headlamp_port": 18466,
	  "files_enabled": true, "files_scope": "/srv", "files_allow_write": true,
	  "podman_enabled": true,
	  "sandbox_enabled": true, "sandbox_max_memory_mb": 2048, "sandbox_cpus": 2,
	  "sandbox_pids_limit": 256, "sandbox_max_containers": 4,
	  "sandbox_allowed_images": ["alpine:3"], "sandbox_scratch_dir": "/tmp/sb",
	  "sandbox_prewarm_images": ["alpine:3"],
	  "ollama_enabled": true, "ollama_pool_enabled": true,
	  "warm_serving_enabled": true, "warm_budget_frac": 0.5,
	  "lan_inference_enabled": true, "lan_inference_port": 11436,
	  "otel_enabled": true, "otel_pool_enabled": true,
	  "ycode_enabled": true, "ycode_share_enabled": true,
	  "ycode_share_require_login": true,
	  "ycode_share_surfaces": {"ycode-canvas": true},
	  "shard": {"enabled": true, "role": "leader", "node_bytes": 17179869184}
	}`)
	if err := os.WriteFile(tmp, stale, 0o600); err != nil {
		t.Fatal(err)
	}
	fc, err := LoadFile(tmp)
	if err != nil {
		t.Fatalf("a config with retired keys must still parse: %v", err)
	}
	if fc.AgentName != "host-1" {
		t.Fatalf("the surviving keys were not read: %+v", fc)
	}
	// Re-saving drops them: nothing in the struct can hold them, so the
	// written file is the new shape.
	if err := SaveFile(tmp, fc); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	saved, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(saved, &raw); err != nil {
		t.Fatalf("parse saved config: %v", err)
	}
	for _, key := range []string{
		"zot_enabled", "zot_port", "seaweedfs_enabled", "seaweedfs_port",
		"kopia_enabled", "kopia_port", "actrunner_enabled", "actrunner_instance",
		"actrunner_token", "actrunner_labels", "actrunner_sandbox",
		"actrunner_sandbox_image", "actrunner_docker_host",
		"headlamp_enabled", "headlamp_port",
		"files_enabled", "files_scope", "files_allow_write",
		"podman_enabled",
		"sandbox_enabled", "sandbox_max_memory_mb", "sandbox_cpus",
		"sandbox_pids_limit", "sandbox_max_containers", "sandbox_allowed_images",
		"sandbox_scratch_dir", "sandbox_prewarm_images",
		"ollama_enabled", "ollama_pool_enabled",
		"warm_serving_enabled", "warm_budget_frac",
		"lan_inference_enabled", "lan_inference_port",
		"otel_enabled", "otel_pool_enabled",
		"ycode_enabled", "ycode_share_enabled", "ycode_share_require_login",
		"ycode_share_surfaces", "shard",
	} {
		if _, ok := raw[key]; ok {
			t.Errorf("retired key %q survived a load/save round trip", key)
		}
	}
}
