package main

import (
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// The opt-in SDLC loop service is present in the defaults, disabled, with a
// Command base so the supervisor calls `bashy sdlc service {start,status,stop}`.
func TestDefaultBashyServicesHasOptInSDLC(t *testing.T) {
	var svc *conf.BashyService
	for _, s := range conf.DefaultBashyServices() {
		if s.Name == "sdlc" {
			s := s
			svc = &s
		}
	}
	if svc == nil {
		t.Fatal("expected a default 'sdlc' service entry")
	}
	if svc.Enabled {
		t.Fatal("the sdlc loop must be opt-in (disabled by default)")
	}
	if len(svc.Command) != 2 || svc.Command[0] != "sdlc" || svc.Command[1] != "service" {
		t.Fatalf("sdlc default Command = %v, want [sdlc service]", svc.Command)
	}
}

// An operator can enable the sdlc loop with just {name,enabled,args}; the Command
// base is inherited from the default so they need not re-declare the argv.
func TestEffectiveBashyServicesInheritsCommand(t *testing.T) {
	fc := &conf.FileConfig{
		BashyServices: []conf.BashyService{
			{Name: "sdlc", Enabled: true, Args: []string{"--config", "/repo/.bashy/sdlc.yaml", "--cwd", "/repo"}},
		},
	}
	var got *conf.BashyService
	for _, s := range effectiveBashyServices(fc) {
		if s.Name == "sdlc" {
			s := s
			got = &s
		}
	}
	if got == nil {
		t.Fatal("sdlc service missing from effective set")
	}
	if !got.Enabled {
		t.Fatal("operator-enabled sdlc should be enabled")
	}
	if len(got.Command) != 2 || got.Command[0] != "sdlc" || got.Command[1] != "service" {
		t.Fatalf("Command not inherited: %v", got.Command)
	}
	if len(got.Args) != 4 {
		t.Fatalf("operator Args lost: %v", got.Args)
	}
}
