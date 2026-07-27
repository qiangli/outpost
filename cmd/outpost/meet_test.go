package main

import (
	"testing"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// findBashyService returns the effective service entry for name, or nil.
func findBashyService(fc *conf.FileConfig, name string) *conf.BashyService {
	for _, s := range effectiveBashyServices(fc) {
		if s.Name == name {
			s := s
			return &s
		}
	}
	return nil
}

// THE TRAP: the meet catalog entry MUST carry Command=["meet","service"] so the
// supervisor invokes `bashy meet service start` (the web chat daemon) — NOT
// `bashy meet start`, which already exists and starts a deliberation session.
// This test pins the Command base so nobody "simplifies" it back to ["meet"].
func TestDefaultBashyServicesMeetCommandPinned(t *testing.T) {
	var svc *conf.BashyService
	for _, s := range conf.DefaultBashyServices() {
		if s.Name == "meet" {
			s := s
			svc = &s
		}
	}
	if svc == nil {
		t.Fatal("expected a default 'meet' service entry")
	}
	if len(svc.Command) != 2 || svc.Command[0] != "meet" || svc.Command[1] != "service" {
		t.Fatalf("meet default Command = %v, want [meet service] (else 'bashy meet start' starts a meeting, not the daemon)", svc.Command)
	}
}

// An operator can enable meet with just {name:"meet",enabled:true}; the Command
// base is inherited from the default so `bashy meet service {start,...}` runs.
func TestEffectiveBashyServicesMeetInheritsCommand(t *testing.T) {
	fc := &conf.FileConfig{
		BashyServices: []conf.BashyService{
			{Name: "meet", Enabled: true},
		},
	}
	got := findBashyService(fc, "meet")
	if got == nil {
		t.Fatal("meet service missing from effective set")
	}
	if !got.Enabled {
		t.Fatal("operator-enabled meet should be enabled")
	}
	if len(got.Command) != 2 || got.Command[0] != "meet" || got.Command[1] != "service" {
		t.Fatalf("Command not inherited: %v, want [meet service]", got.Command)
	}
}

// meet_enabled=true yields a meet service registered as a cloudbox app at
// http://127.0.0.1:8637 with RequireLogin=true and NO mesh service.
func TestEffectiveBashyServicesMeetOn(t *testing.T) {
	on := true
	fc := &conf.FileConfig{MeetEnabled: &on}
	got := findBashyService(fc, "meet")
	if got == nil {
		t.Fatal("meet service missing from effective set when meet_enabled=true")
	}
	if !got.Enabled {
		t.Fatal("meet should be enabled when meet_enabled=true")
	}
	if got.AppName != "meet" {
		t.Fatalf("AppName = %q, want meet", got.AppName)
	}
	if got.AppPort != 8637 {
		t.Fatalf("AppPort = %d, want 8637", got.AppPort)
	}
	if !got.RequireLogin {
		t.Fatal("meet must RequireLogin")
	}
	if got.MeshService != "" {
		t.Fatalf("meet must NOT expose a mesh service, got %q", got.MeshService)
	}

	// registerBashyServiceApps must publish it under name "meet".
	reg := agent.NewAppRegistry()
	if err := registerBashyServiceApps(fc, reg); err != nil {
		t.Fatalf("registerBashyServiceApps: %v", err)
	}
	u := reg.LookupTarget("meet")
	if u == nil {
		t.Fatal("meet app not registered")
	}
	if u.String() != "http://127.0.0.1:8637" {
		t.Fatalf("meet target = %q, want http://127.0.0.1:8637", u.String())
	}
	var requireLogin bool
	for _, e := range reg.Entries() {
		if e.Name == "meet" {
			requireLogin = e.RequireLogin
		}
	}
	if !requireLogin {
		t.Fatal("meet app must be registered with RequireLogin=true")
	}
}

// Default OFF: a config with no meet_enabled key registers NO meet app and
// yields no enabled meet service.
func TestEffectiveBashyServicesMeetDefaultOff(t *testing.T) {
	fc := &conf.FileConfig{} // no meet_enabled
	if got := findBashyService(fc, "meet"); got != nil && got.Enabled {
		t.Fatalf("meet must default OFF, got enabled entry: %+v", got)
	}
	reg := agent.NewAppRegistry()
	if err := registerBashyServiceApps(fc, reg); err != nil {
		t.Fatalf("registerBashyServiceApps: %v", err)
	}
	if u := reg.LookupTarget("meet"); u != nil {
		t.Fatalf("no meet app should be registered by default, got %q", u.String())
	}
}

// meet_port override flows through to the registered app target.
func TestEffectiveBashyServicesMeetPortOverride(t *testing.T) {
	on := true
	fc := &conf.FileConfig{MeetEnabled: &on, MeetPort: 9999}
	got := findBashyService(fc, "meet")
	if got == nil {
		t.Fatal("meet service missing")
	}
	if got.AppPort != 9999 {
		t.Fatalf("AppPort = %d, want 9999 (override)", got.AppPort)
	}
	reg := agent.NewAppRegistry()
	if err := registerBashyServiceApps(fc, reg); err != nil {
		t.Fatalf("registerBashyServiceApps: %v", err)
	}
	u := reg.LookupTarget("meet")
	if u == nil || u.String() != "http://127.0.0.1:9999" {
		t.Fatalf("meet target = %v, want http://127.0.0.1:9999", u)
	}
}
