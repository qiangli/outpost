package conf

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	if !out.ShellOn() || !out.DesktopOn() || !out.ClipboardOn() || !out.SSHOn() {
		t.Errorf("legacy config should default-on: shell=%v desktop=%v clipboard=%v ssh=%v",
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
		{"ollama-off", &FileConfig{OllamaEnabled: false}, false},
		{"ollama-on-pool-unset", &FileConfig{OllamaEnabled: true}, true},
		{"ollama-on-pool-explicit-on", &FileConfig{OllamaEnabled: true, OllamaPoolEnabled: &on}, true},
		{"ollama-on-pool-explicit-off", &FileConfig{OllamaEnabled: true, OllamaPoolEnabled: &off}, false},
		{"ollama-off-pool-on-ignored", &FileConfig{OllamaEnabled: false, OllamaPoolEnabled: &on}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fc.OllamaPoolOn(); got != tt.want {
				t.Errorf("OllamaPoolOn()=%v, want %v", got, tt.want)
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
// vk-podman, while the three canonical modes round-trip unchanged.
func TestNormalizeClusterMode(t *testing.T) {
	cases := map[string]string{
		"":           ClusterModeVKPodman,
		"vkpodman":   ClusterModeVKPodman,
		"VKPodman":   ClusterModeVKPodman,
		" vkpodman ": ClusterModeVKPodman,
		"vk-podman":  ClusterModeVKPodman,
		"agent":      ClusterModeAgentMode,
		"AGENT":      ClusterModeAgentMode,
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
// "" select neither agent nor vk-ollama (i.e. the libpod vk-podman
// backend).
func TestClusterModeHelpers(t *testing.T) {
	for _, mode := range []string{"", "vkpodman", "vk-podman"} {
		c := &ClusterConfig{Mode: mode}
		if c.ClusterModeAgent() {
			t.Errorf("Mode=%q: ClusterModeAgent() = true, want false", mode)
		}
		if c.ClusterModeVKOllama() {
			t.Errorf("Mode=%q: ClusterModeVKOllama() = true, want false", mode)
		}
		if c.ClusterMode() != ClusterModeVKPodman {
			t.Errorf("Mode=%q: ClusterMode() = %q, want vk-podman", mode, c.ClusterMode())
		}
	}
	if c := (&ClusterConfig{Mode: "agent"}); !c.ClusterModeAgent() || c.ClusterModeVKOllama() {
		t.Errorf("Mode=agent: helpers wrong (agent=%v ollama=%v)", c.ClusterModeAgent(), c.ClusterModeVKOllama())
	}
	if c := (&ClusterConfig{Mode: "vk-ollama"}); !c.ClusterModeVKOllama() || c.ClusterModeAgent() {
		t.Errorf("Mode=vk-ollama: helpers wrong (agent=%v ollama=%v)", c.ClusterModeAgent(), c.ClusterModeVKOllama())
	}
	// nil receiver normalizes like an empty Mode.
	var nilc *ClusterConfig
	if nilc.ClusterMode() != ClusterModeVKPodman || nilc.ClusterModeAgent() || nilc.ClusterModeVKOllama() {
		t.Errorf("nil receiver: helpers wrong")
	}
}
