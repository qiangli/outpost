package postinstall

import (
	"context"
	"strings"
	"testing"
)

func TestPowerCommands(t *testing.T) {
	for _, tt := range []struct {
		os   string
		want []string
	}{
		{"windows", []string{"powercfg /change standby-timeout-ac 0", "powercfg /change standby-timeout-dc 0", "powercfg /change hibernate-timeout-ac 0", "powercfg /change hibernate-timeout-dc 0", "powercfg /hibernate off"}},
		{"darwin", []string{"pmset -a sleep 0 disksleep 0 womp 1"}},
		{"linux", []string{"systemctl mask sleep.target suspend.target hibernate.target hybrid-sleep.target"}},
	} {
		t.Run(tt.os, func(t *testing.T) {
			got := PowerCommands(tt.os)
			if len(got) != len(tt.want) {
				t.Fatalf("commands = %v", got)
			}
			for i, c := range got {
				if strings.Join(c, " ") != tt.want[i] {
					t.Errorf("command = %v", c)
				}
			}
		})
	}
}

func TestPowerReadbacks(t *testing.T) {
	for _, tt := range []struct {
		name, os string
		raw      map[string]Readback
		want     bool
	}{
		{"mac all sources", "darwin", map[string]Readback{"power": {Output: "Battery Power:\n sleep 0\n disksleep 0\n womp 1\nAC Power:\n sleep 0\n disksleep 0\n womp 1\n"}}, true},
		{"mac battery sleeps", "darwin", map[string]Readback{"power": {Output: "Battery Power:\n sleep 1\n disksleep 0\n womp 1\nAC Power:\n sleep 0\n disksleep 0\n womp 1\n"}}, false},
		{"mac missing", "darwin", map[string]Readback{"power": {Output: "AC Power:\n displaysleep 0\n"}}, false},
		{"windows ac dc", "windows", map[string]Readback{"standby": {Output: "AC: 0x00000000\nDC: 0x00000000"}, "hibernate": {Output: "AC: 0x00000000\nDC: 0x00000000"}, "hibernate_enabled": {Output: "0"}}, true},
		{"windows dc sleeps", "windows", map[string]Readback{"standby": {Output: "AC: 0x00000000\nDC: 0x00000001"}, "hibernate": {Output: "AC: 0x00000000\nDC: 0x00000000"}, "hibernate_enabled": {Output: "0"}}, false},
		{"windows incomplete", "windows", map[string]Readback{"standby": {Output: "AC: 0x00000000"}}, false},
		{"linux persistent masks", "linux", linuxMasks("masked"), true},
		{"linux temporary masks", "linux", linuxMasks("masked-runtime"), false},
		{"linux errors", "linux", map[string]Readback{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := PowerDisabled(tt.os, tt.raw); got != tt.want {
				t.Fatalf("disabled=%v want %v", got, tt.want)
			}
		})
	}
}
func linuxMasks(s string) map[string]Readback {
	m := map[string]Readback{}
	for _, target := range sleepTargets {
		m[target] = Readback{Output: s, ExitCode: 1}
	}
	return m
}

func TestServiceReadbacks(t *testing.T) {
	for _, tt := range []struct {
		name, os string
		raw      map[string]Readback
		want     string
	}{
		{"linux boot", "linux", map[string]Readback{"system_service": {Output: "enabled"}}, "boot"},
		{"linux runtime", "linux", map[string]Readback{"system_service": {Output: "enabled-runtime"}}, "session"},
		{"linux user linger", "linux", map[string]Readback{"user_service": {Output: "enabled"}, "linger": {Output: "yes"}}, "boot"},
		{"linux user no linger", "linux", map[string]Readback{"user_service": {Output: "enabled"}}, "session"},
		{"mac daemon", "darwin", map[string]Readback{"system_service": {Output: `{"Label":"io.dhnt.outpost","RunAtLoad":true,"KeepAlive":true}`}, "disabled_services": {Output: `disabled services = {}`}}, "boot"},
		{"mac disabled", "darwin", map[string]Readback{"system_service": {Output: `{"Label":"io.dhnt.outpost","RunAtLoad":true,"KeepAlive":true}`}, "disabled_services": {Output: `"io.dhnt.outpost" => true`}}, "none"},
		{"windows startup", "windows", map[string]Readback{"task": {Output: `{"Enabled":true,"Boot":true,"Logon":"S4U"}`}}, "boot"},
		{"windows interactive", "windows", map[string]Readback{"task": {Output: `{"Enabled":true,"Boot":true,"Logon":"Interactive"}`}}, "session"},
		{"windows disabled", "windows", map[string]Readback{"task": {Output: `{"Enabled":false,"Boot":true,"Logon":"S4U"}`}}, "none"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ServiceState(tt.os, tt.raw); got != tt.want {
				t.Errorf("service=%s want %s", got, tt.want)
			}
		})
	}
}

func TestApplyPowerFailureAndDryRun(t *testing.T) {
	calls := 0
	run := func(_ context.Context, c Command) Readback {
		calls++
		return Readback{Command: c, ExitCode: 1, Error: "access denied"}
	}
	err := ApplyPower(context.Background(), "windows", run, false, nil)
	if err == nil || !strings.Contains(err.Error(), "powercfg /change standby-timeout-ac 0") {
		t.Fatalf("missing remediation: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	calls = 0
	if err := ApplyPower(context.Background(), "darwin", run, true, nil); err != nil || calls != 0 {
		t.Fatal("dry run mutated power")
	}
}

func TestRebootEvidence(t *testing.T) {
	for _, tt := range []struct {
		name                                 string
		boot, service, login, origin, online bool
		want                                 string
	}{
		{"all", true, true, true, true, true, "PASS"},
		{"same boot", false, true, true, true, true, "FAIL"},
		{"login", true, true, false, true, true, "FAIL"},
		{"manual", true, true, true, false, true, "FAIL"},
		{"offline", true, true, true, true, false, "FAIL"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := Report{Sleep: "disabled", Service: "boot", Raw: map[string]Readback{}}
			r.ApplyLocal(LocalChecks{SupervisorRunning: true, DaemonHealthy: true, SSHDListening: true, BootPathSane: true})
			if !tt.service {
				r.Service = "session"
				r.Evaluate()
			}
			b := RebootEvidence{BootChanged: tt.boot, NoLogin: tt.login, ServiceLaunch: tt.origin, ServerAcknowledged: tt.online}
			r.Reboot = &b
			r.Evaluate()
			if r.Result != tt.want {
				t.Fatalf("%s", r.Result)
			}
		})
	}
}

// TestApplyLocalGatesEveryCheck — each of the six post-install checks
// fails the verdict on its own. Same matrix on macOS, Linux, Windows:
// the probes differ per platform but the gate doesn't.
func TestApplyLocalGatesEveryCheck(t *testing.T) {
	good := LocalChecks{SupervisorRunning: true, DaemonHealthy: true, SSHDListening: true, BootPathSane: true}
	for _, tt := range []struct {
		name   string
		mutate func(*LocalChecks)
		sleep  string
		svc    string
	}{
		{"all good", func(*LocalChecks) {}, "disabled", "boot"},
		{"sleep enabled", func(*LocalChecks) {}, "enabled", "boot"},
		{"service session", func(*LocalChecks) {}, "disabled", "session"},
		{"supervisor down", func(c *LocalChecks) { c.SupervisorRunning = false }, "disabled", "boot"},
		{"daemon down", func(c *LocalChecks) { c.DaemonHealthy = false }, "disabled", "boot"},
		{"sshd closed", func(c *LocalChecks) { c.SSHDListening = false }, "disabled", "boot"},
		{"boot PATH broken", func(c *LocalChecks) {
			c.BootPathSane = false
			c.BootPathDetail = "missing /usr/bin"
		}, "disabled", "boot"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := good
			tt.mutate(&c)
			r := Report{Sleep: tt.sleep, Service: tt.svc, Raw: map[string]Readback{}}
			r.ApplyLocal(c)
			want := "FAIL"
			if tt.name == "all good" {
				want = "PASS"
			}
			if r.Result != want {
				t.Fatalf("result = %q, want %q (%+v)", r.Result, want, r)
			}
			if tt.name == "boot PATH broken" && r.BootPathDetail == "" {
				t.Fatal("BootPathDetail should carry the evidence")
			}
		})
	}
}
