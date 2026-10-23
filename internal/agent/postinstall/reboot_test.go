package postinstall

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func loadReport(t *testing.T, dir, name string) Report {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(dir, name))
	if e != nil {
		t.Fatal(e)
	}
	var r Report
	if e = json.Unmarshal(b, &r); e != nil {
		t.Fatal(e)
	}
	return r
}

// fakeHost covers the complete OS readback pipeline, never running host tools.
func fakeHost(platform string, boot *string, login *bool) Runner {
	return func(_ context.Context, c Command) Readback {
		r := Readback{Command: c}
		if reflect.DeepEqual(c, BootCommand(platform)) {
			r.Output = *boot
			return r
		}
		if reflect.DeepEqual(c, RebootCommand(platform)) {
			return r
		}
		switch platform {
		case "linux":
			switch strings.Join(c, " ") {
			case "systemctl is-enabled outpost.service":
				r.Output = "enabled"
			case "systemctl --user is-enabled outpost.service":
				r.Output = "disabled"
				r.ExitCode = 1
			case "loginctl show-user fixture --property=Linger --value":
				r.Output = "no"
			case "systemctl show outpost.service --property=MainPID --value":
				r.Output = "42"
			case "loginctl list-sessions --no-legend --no-pager":
				if *login {
					r.Output = "7 1000 fixture seat0 tty1"
				}
			case "loginctl show-session 7 --property=Class --value":
				r.Output = "user"
			default:
				if len(c) == 3 && c[1] == "is-enabled" {
					r.Output = "masked"
					r.ExitCode = 1
				} else {
					r.Error = "unexpected command"
					r.ExitCode = 1
				}
			}
		case "darwin":
			switch c[0] {
			case "pmset":
				r.Output = "AC Power:\n sleep 0\n disksleep 0\n womp 1\n"
			case "plutil":
				if strings.Contains(c[len(c)-1], "LaunchDaemons") {
					r.Output = `{"Label":"io.dhnt.outpost","RunAtLoad":true,"KeepAlive":true}`
				} else {
					r.ExitCode = 1
				}
			case "launchctl":
				if c[1] == "print-disabled" {
					r.Output = "disabled services = {}"
				} else {
					r.Output = " pid = 42\n"
				}
			case "who":
				if *login {
					r.Output = "fixture console"
				}
			case "stat":
				r.Output = "0"
				if *login {
					r.Output = "1000"
				}
			default:
				r.ExitCode = 1
				r.Error = "unexpected command"
			}
		case "windows":
			if c[0] == "powercfg" {
				r.Output = "AC: 0x00000000\nDC: 0x00000000"
				return r
			}
			body := c[len(c)-1]
			switch {
			case strings.Contains(body, "HibernateEnabled"):
				r.Output = "0"
			case strings.Contains(body, "Get-ScheduledTask"):
				r.Output = `{"Enabled":true,"Boot":true,"Logon":"S4U"}`
			case strings.Contains(body, "Win32_Process"):
				r.Output = `{"Session":0,"Supervisor":true}`
			case strings.Contains(body, "Win32_LogonSession"):
				r.Output = "0"
				if *login {
					r.Output = "1"
				}
			default:
				r.ExitCode = 1
				r.Error = "unexpected command"
			}
		}
		return r
	}
}

func TestRebootLifecycleAllOS(t *testing.T) {
	for _, platform := range []string{"windows", "darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			boot := "first-boot"
			login := false
			run := fakeHost(platform, &boot, &login)
			r := Read(ctx, platform, "fixture", "/fixture", run)
			if r.Result != "PASS" {
				t.Fatalf("initial: %+v", r)
			}
			if e := Schedule(ctx, dir, r, run); e != nil {
				t.Fatal(e)
			}
			if got := loadReport(t, dir, RebootReportName); got.Result != "FAIL" || got.Reboot.Phase != "awaiting_reboot" {
				t.Fatalf("premature pass: %+v", got)
			}
			o, e := Resume(ctx, dir, platform, "fixture", "/fixture", 42, run)
			if e != nil || o != nil {
				t.Fatalf("same boot should wait: %v %v", o, e)
			}
			boot = "second-boot"
			o, e = Resume(ctx, dir, platform, "fixture", "/fixture", 42, run)
			if e != nil || o == nil {
				t.Fatalf("resume: %v", e)
			}
			if got := loadReport(t, dir, RebootReportName); got.Result != "FAIL" || !got.Reboot.NoLogin || !got.Reboot.ServiceLaunch {
				t.Fatalf("startup: %+v", got)
			}
			if e = o.Acknowledge(ctx); e != nil {
				t.Fatal(e)
			}
			got := loadReport(t, dir, RebootReportName)
			if got.Result != "PASS" || !got.Reboot.ServerAcknowledged || got.Reboot.AcknowledgedAt == nil {
				t.Fatalf("ack: %+v", got)
			}
			if _, e = os.Stat(filepath.Join(dir, PendingName)); !os.IsNotExist(e) {
				t.Fatal("pending marker remains")
			}
			if e = o.Acknowledge(ctx); e != nil {
				t.Fatal("ack must be idempotent", e)
			}
		})
	}
}

func TestLoginCannotBeErasedByRestart(t *testing.T) {
	for _, platform := range []string{"windows", "darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			boot := "before"
			login := false
			run := fakeHost(platform, &boot, &login)
			if e := Schedule(ctx, dir, Read(ctx, platform, "fixture", "/fixture", run), run); e != nil {
				t.Fatal(e)
			}
			boot = "after"
			login = true
			if _, e := Resume(ctx, dir, platform, "fixture", "/fixture", 42, run); e != nil {
				t.Fatal(e)
			}
			login = false
			o, e := Resume(ctx, dir, platform, "fixture", "/fixture", 42, run)
			if e != nil {
				t.Fatal(e)
			}
			if e = o.Acknowledge(ctx); e != nil {
				t.Fatal(e)
			}
			if got := loadReport(t, dir, RebootReportName); got.Result != "FAIL" || got.Reboot.NoLogin {
				t.Fatal("logout erased login evidence")
			}
		})
	}
}

func TestRebootFailurePaths(t *testing.T) {
	ctx := context.Background()
	t.Run("bad policy never schedules", func(t *testing.T) {
		calls := 0
		e := Schedule(ctx, t.TempDir(), Report{Result: "FAIL"}, func(context.Context, Command) Readback { calls++; return Readback{} })
		if e == nil || calls != 0 {
			t.Fatal("reboot attempted with failed checks")
		}
	})
	t.Run("no marker no probes", func(t *testing.T) {
		o, e := Resume(ctx, t.TempDir(), "linux", "fixture", "/fixture", 42, func(context.Context, Command) Readback { t.Fatal("unexpected probe"); return Readback{} })
		if e != nil || o != nil {
			t.Fatal(o, e)
		}
	})
	t.Run("schedule denied retains fail and retry", func(t *testing.T) {
		dir := t.TempDir()
		boot := "before"
		login := false
		fake := fakeHost("linux", &boot, &login)
		run := func(ctx context.Context, c Command) Readback {
			if c[0] == "sudo" {
				return Readback{ExitCode: 1, Error: "denied"}
			}
			return fake(ctx, c)
		}
		e := Schedule(ctx, dir, Read(ctx, "linux", "fixture", "/fixture", run), run)
		if e == nil || !strings.Contains(e.Error(), "sudo shutdown -r +1") {
			t.Fatal(e)
		}
		got := loadReport(t, dir, RebootReportName)
		if got.Result != "FAIL" || got.Reboot.Phase != "schedule_failed" {
			t.Fatal(got)
		}
	})
	t.Run("write failure never reboots", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "file")
		if e := os.WriteFile(dir, []byte("x"), 0600); e != nil {
			t.Fatal(e)
		}
		boot := "before"
		login := false
		fake := fakeHost("linux", &boot, &login)
		run := func(ctx context.Context, c Command) Readback {
			if c[0] == "sudo" {
				t.Fatal("reboot despite lost evidence")
			}
			return fake(ctx, c)
		}
		if e := Schedule(ctx, dir, Read(ctx, "linux", "fixture", "/fixture", run), run); e == nil {
			t.Fatal("expected write failure")
		}
	})
}

func TestRebootCommandPlans(t *testing.T) {
	for _, tt := range []struct {
		platform     string
		boot, reboot Command
	}{
		{"linux", Command{"cat", "/proc/sys/kernel/random/boot_id"}, Command{"sudo", "-n", "shutdown", "-r", "+1"}},
		{"darwin", Command{"sysctl", "-n", "kern.boottime"}, Command{"sudo", "-n", "shutdown", "-r", "+1"}},
		{"windows", ps("(Get-CimInstance Win32_OperatingSystem).LastBootUpTime.ToUniversalTime().ToString('o')"), Command{"shutdown.exe", "/r", "/t", "60"}},
	} {
		t.Run(tt.platform, func(t *testing.T) {
			if !reflect.DeepEqual(BootCommand(tt.platform), tt.boot) || !reflect.DeepEqual(RebootCommand(tt.platform), tt.reboot) {
				t.Fatal("wrong reboot command plan")
			}
		})
	}
}
