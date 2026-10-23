package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/postinstall"
)

func TestServiceInstallPolicy(t *testing.T) {
	for _, tt := range []struct {
		name                                   string
		args                                   []string
		failPower, system, power, dry, wantErr bool
	}{
		{name: "default boot and no sleep", system: true, power: true},
		{name: "keep sleep", args: []string{"--keep-sleep"}, system: true},
		{name: "dry run", args: []string{"--dry-run"}, system: true, dry: true},
		{name: "explicit user", args: []string{"--user", "--keep-sleep"}},
		{name: "power denied", failPower: true, power: true, wantErr: true},
		{name: "conflicting flags", args: []string{"--user", "--system"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			powerCalls, installs := 0, 0
			var got installOpts
			cmd := newServiceInstallCmd(func(_ context.Context, c postinstall.Command) postinstall.Readback {
				powerCalls++
				if tt.failPower {
					return postinstall.Readback{ExitCode: 1, Error: "access denied"}
				}
				return postinstall.Readback{}
			}, func(opts installOpts) error { installs++; got = opts; return nil })
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if (err != nil) != tt.wantErr {
				t.Fatalf("error=%v", err)
			}
			if (powerCalls > 0) != tt.power {
				t.Errorf("power calls=%d", powerCalls)
			}
			if tt.wantErr {
				if installs != 0 {
					t.Error("installed after failure")
				}
				return
			}
			if installs != 1 || got.System != tt.system || got.DryRun != tt.dry {
				t.Fatalf("install=%d %+v", installs, got)
			}
			if tt.dry && !strings.Contains(output.String(), "# ") {
				t.Error("missing power preview")
			}
		})
	}
}

func TestServiceRenderEscaping(t *testing.T) {
	assertContains(t, "plist", renderLaunchDaemonPlist("/Apps/A&B/outpost", "fixture", "/home/A&B"), "/Apps/A&amp;B/outpost", "/home/A&amp;B")
	assertContains(t, "task", renderWindowsStartupTask(`C:\A'B\outpost.exe`, `FIXTURE\User`), `C:\A''B\outpost.exe`, "$ErrorActionPreference = 'Stop'")
	assertContains(t, "unit", renderSystemdSystemUnit("/opt/a b/100%/outpost", "fixture"), `ExecStart="/opt/a b/100%%/outpost" supervisord`)
}

func TestServiceRegistrationTakeover(t *testing.T) {
	for _, tt := range []struct {
		name                          string
		before, after, force, wantErr bool
	}{
		{name: "dormant login service"},
		{name: "active managed service", before: true},
		{name: "foreign launcher", before: true, after: true, wantErr: true},
		{name: "explicit force", before: true, after: true, force: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			probes, removals, waits := 0, 0, 0
			err := reconcileServiceRegistration(installOpts{System: true, Force: tt.force}, func() bool {
				probes++
				if probes == 1 {
					return tt.before
				}
				return tt.after
			}, func(installOpts) { removals++ }, func(time.Duration) { waits++ })
			if (err != nil) != tt.wantErr {
				t.Fatalf("error=%v", err)
			}
			if removals != 1 {
				t.Fatal("dormant registration was not reconciled")
			}
			if (waits > 0) != tt.before {
				t.Fatal("unexpected shutdown wait")
			}
		})
	}
}
