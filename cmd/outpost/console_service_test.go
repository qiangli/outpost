package main

import (
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
)

func consoleService(t *testing.T, svcs []conf.BashyService) conf.BashyService {
	t.Helper()
	for _, s := range svcs {
		if s.Name == "console" {
			return s
		}
	}
	t.Fatal("no 'console' service entry")
	return conf.BashyService{}
}

// The console is present, opt-in, and carries the subcommand base — the same
// trap meet has: bare `bashy apps` serves in the FOREGROUND, so a supervisor
// calling it would block forever instead of daemonising.
func TestDefaultBashyServicesHasOptInConsole(t *testing.T) {
	svc := consoleService(t, conf.DefaultBashyServices())
	if svc.Enabled {
		t.Error("the console must be opt-in (disabled by default)")
	}
	if len(svc.Command) != 2 || svc.Command[0] != "apps" || svc.Command[1] != "service" {
		t.Errorf("console Command = %v, want [apps service] — bare `bashy apps` is a foreground human verb", svc.Command)
	}
	if svc.AppName != "console" {
		t.Errorf("console AppName = %q, want %q — outpost already serves an unrelated GET /apps", svc.AppName, "console")
	}
	if svc.AppPort != 8639 {
		t.Errorf("console AppPort = %d, want 8639", svc.AppPort)
	}
	if !svc.RequireLogin || !svc.TrustCloudIdentity {
		t.Errorf("console published with RequireLogin=%v TrustCloudIdentity=%v; both must hold — this app can hand out a shell",
			svc.RequireLogin, svc.TrustCloudIdentity)
	}
}

// TrustCloudIdentity is only safe where the vouch is HMAC-verified: without an
// SSO secret coopauth admits on the header alone. registerBashyServiceApps
// already refuses that combination, and for the console — which routes a
// Terminal that spawns a real bashy as this OS user — that refusal is the whole
// security argument, so it is pinned here.
func TestConsoleWithoutSSOSecretIsRefused(t *testing.T) {
	on := true
	fc := &conf.FileConfig{ConsoleEnabled: &on}
	reg := agent.NewAppRegistry()
	err := registerBashyServiceApps(fc, reg)
	if err == nil {
		t.Fatal("registering the console with no sso_secret succeeded; it must be refused")
	}
	if !strings.Contains(err.Error(), "sso_secret") {
		t.Fatalf("refusal = %v, want it to name the missing sso_secret", err)
	}
}

// Enabling the console yields a supervised, published app on the configured port.
func TestConsoleOnPublishesTheApp(t *testing.T) {
	on := true
	fc := &conf.FileConfig{
		ConsoleEnabled: &on,
		ConsolePort:    18639,
		BashyServices:  []conf.BashyService{{Name: "console", SSOSecret: "s3cr3t"}},
	}
	svc := consoleService(t, effectiveBashyServices(fc))
	if !svc.Enabled || svc.AppPort != 18639 || svc.AppName != "console" {
		t.Fatalf("console = %+v, want enabled on 18639 as `console`", svc)
	}
	reg := agent.NewAppRegistry()
	if err := registerBashyServiceApps(fc, reg); err != nil {
		t.Fatalf("registerBashyServiceApps: %v", err)
	}
	if target := reg.LookupTarget("console"); target == nil {
		t.Fatal("the console app was not registered")
	}
}

// A disabled panel must reach the SERVER as a flag, not merely be hidden in the
// admin UI: HostShare grants are per app name, so publishing `console` publishes
// every panel it routes. The console enforces --disable by not routing them.
func TestConsoleDisableReachesTheServerArgv(t *testing.T) {
	on := true
	fc := &conf.FileConfig{
		ConsoleEnabled: &on,
		ConsoleDisable: []string{"terminal", " files "},
		BashyServices:  []conf.BashyService{{Name: "console", SSOSecret: "s3cr3t"}},
	}
	got := strings.Join(consoleService(t, effectiveBashyServices(fc)).Args, " ")
	for _, want := range []string{"--disable terminal", "--disable files"} {
		if !strings.Contains(got, want) {
			t.Errorf("console args = %q, want %q — a panel hidden but still routed is not disabled", got, want)
		}
	}
}
