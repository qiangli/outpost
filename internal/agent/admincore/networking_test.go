package admincore

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
)

func newTestCore(t *testing.T) (*Server, string) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(cfgPath, &conf.FileConfig{AgentName: "host1", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	core, err := New(Deps{
		ConfigPath: cfgPath,
		Apps:       agent.NewAppRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return core, cfgPath
}

func TestSetNetworking_PointerSemantics(t *testing.T) {
	core, cfgPath := newTestCore(t)
	// Set vnc_addr, leave others nil.
	v := "192.168.1.5:5900"
	res, err := core.SetNetworking(NetworkingParams{VNCAddr: &v})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !res.OK || !res.RestartPending {
		t.Errorf("want OK + RestartPending, got %+v", res)
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fc.VNCAddr != v {
		t.Errorf("VNCAddr = %q, want %q", fc.VNCAddr, v)
	}
	if fc.LocalAddr != "" {
		t.Errorf("LocalAddr unexpectedly set to %q", fc.LocalAddr)
	}
}

func TestSetNetworking_ClearString(t *testing.T) {
	core, cfgPath := newTestCore(t)
	// Establish a value.
	a := "0.0.0.0:17777"
	if _, err := core.SetNetworking(NetworkingParams{AdminAddr: &a}); err != nil {
		t.Fatal(err)
	}
	// Now clear it (pointer to empty string).
	empty := ""
	if _, err := core.SetNetworking(NetworkingParams{AdminAddr: &empty}); err != nil {
		t.Fatal(err)
	}
	fc, _ := conf.LoadFile(cfgPath)
	if fc.AdminAddr != "" {
		t.Errorf("AdminAddr = %q, want empty (cleared)", fc.AdminAddr)
	}
}

func TestSetNetworking_AdminUsersNormalization(t *testing.T) {
	core, cfgPath := newTestCore(t)
	users := []string{"  Alice@example.com ", "bob@example.com", "alice@example.com", ""}
	res, err := core.SetNetworking(NetworkingParams{AdminUsers: &users})
	if err != nil {
		t.Fatal(err)
	}
	if !res.RestartPending {
		t.Error("expected RestartPending")
	}
	fc, _ := conf.LoadFile(cfgPath)
	want := []string{"alice@example.com", "bob@example.com"}
	if !reflect.DeepEqual(fc.AdminUsers, want) {
		t.Errorf("AdminUsers = %v, want %v", fc.AdminUsers, want)
	}
}

func TestSetNetworking_NoChangeNoRestart(t *testing.T) {
	core, _ := newTestCore(t)
	a := "127.0.0.1:17777"
	if _, err := core.SetNetworking(NetworkingParams{AdminAddr: &a}); err != nil {
		t.Fatal(err)
	}
	// Setting it to the same value again should NOT restart.
	res, err := core.SetNetworking(NetworkingParams{AdminAddr: &a})
	if err != nil {
		t.Fatal(err)
	}
	if res.RestartPending {
		t.Error("unchanged save shouldn't trigger restart")
	}
}

func TestSetNetworking_UnpairedNoRestart(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(cfgPath, &conf.FileConfig{}); err != nil { // no AgentName
		t.Fatal(err)
	}
	core, err := New(Deps{ConfigPath: cfgPath, Apps: agent.NewAppRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	a := "0.0.0.0:18888"
	res, err := core.SetNetworking(NetworkingParams{AdminAddr: &a})
	if err != nil {
		t.Fatal(err)
	}
	if res.RestartPending {
		t.Error("unpaired host shouldn't restart on save")
	}
}
