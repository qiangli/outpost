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
