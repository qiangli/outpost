//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	psExe       = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	schtasksExe = `C:\Windows\System32\schtasks.exe`
	powercfgExe = `C:\Windows\System32\powercfg.exe`
)

// startTask launches the registered task on demand. It uses the
// Start-ScheduledTask cmdlet, NOT `schtasks /Run`: the latter returns a false
// LastTaskResult=0xFFFFFFFF and spawns no process while the task is perfectly
// fine. A real boot fires the task via the Task Scheduler service (the same
// path as the cmdlet), so a task that launches via Start-ScheduledTask will run
// at boot. See docs/windows-service.md.
func startTask() error {
	out, err := exec.Command(psExe, "-NoProfile", "-Command",
		"Start-ScheduledTask -TaskName '"+windowsTask+"'").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// disableSleep turns off idle sleep + hibernate so a sleeping host doesn't drop
// the daemon and tunnel — an always-on agent on a host that sleeps defeats the
// purpose. Best-effort and non-fatal.
func disableSleep() {
	for _, a := range []string{"standby-timeout-ac", "standby-timeout-dc", "hibernate-timeout-ac", "hibernate-timeout-dc"} {
		_ = exec.Command(powercfgExe, "/change", a, "0").Run()
	}
}

// windowsUserID is DOMAIN\User (or just User) — the principal the task runs as.
func windowsUserID() string {
	if d := os.Getenv("USERDOMAIN"); d != "" {
		return d + `\` + os.Getenv("USERNAME")
	}
	return os.Getenv("USERNAME")
}

func installService(opts installOpts) error {
	self, err := serviceTarget()
	if err != nil {
		return err
	}
	user := opts.RunAs
	if user == "" {
		user = windowsUserID()
	}
	var body, kind string
	if opts.System {
		body = renderWindowsStartupTask(self, user)
		kind = "at boot"
	} else {
		body = renderWindowsLogonTask(self, user)
		kind = "at logon"
	}
	if opts.DryRun {
		fmt.Printf("# %s -NoProfile -Command \"%s\"\n# (start now) %s -NoProfile -Command \"Start-ScheduledTask -TaskName '%s'\"\n", psExe, body, psExe, windowsTask)
		if opts.System {
			fmt.Printf("# (keep-awake) %s /change standby-timeout-ac 0  (+ -dc, hibernate-timeout-ac/dc)\n", powercfgExe)
		}
		return nil
	}
	if opts.System && !isWindowsAdmin() {
		return fmt.Errorf("system service install needs Administrator — re-run from an elevated prompt:\n  Start-Process %s -ArgumentList 'service install' -Verb RunAs\n(or use --user for a no-admin per-logon install)", self)
	}
	if err := preflightTakeover(opts); err != nil {
		return err
	}
	out, err := exec.Command(psExe, "-NoProfile", "-Command", body).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Register-ScheduledTask failed: %w\n%s", err, out)
	}
	if opts.System {
		// A host that sleeps drops the daemon + tunnel — keep it awake.
		disableSleep()
	}
	// -AtStartup / -AtLogOn won't fire until the next boot/logon — kick the task
	// off now so `service install` is immediately effective (matches the
	// launchd/systemd paths, which start on install via RunAtLoad / --now).
	// Start-ScheduledTask, NOT `schtasks /Run` (see startTask).
	if err2 := startTask(); err2 != nil {
		fmt.Printf("note: task registered but immediate start failed (it will start %s): %v\n", kind, err2)
	}
	fmt.Printf("registered Task Scheduler task %q — runs `outpost supervisord` %s as %q (started now)\n", windowsTask, kind, user)
	return nil
}

// removeManagedRegistrations stops a daemon this binary manages so the new
// supervisor can claim the singleton pidfile. Both modes use the same task name
// (Register -Force replaces it), but replacing the task does not stop an
// already-running daemon — `outpost stop` does. Best-effort and quiet.
func removeManagedRegistrations(_ installOpts) {
	if self, err := serviceTarget(); err == nil {
		_ = exec.Command(self, "stop").Run()
	}
}

func uninstallService(opts installOpts) error {
	if opts.System && !isWindowsAdmin() {
		return fmt.Errorf("system service uninstall needs Administrator — re-run from an elevated prompt")
	}
	out, err := exec.Command(schtasksExe, "/Delete", "/TN", windowsTask, "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks /Delete failed: %w\n%s", err, out)
	}
	fmt.Printf("unregistered Task Scheduler task %q\n", windowsTask)
	return nil
}

func statusService(_ installOpts) error {
	out, err := exec.Command(schtasksExe, "/Query", "/TN", windowsTask).CombinedOutput()
	if err != nil {
		fmt.Printf("Task Scheduler task %q: not registered\n", windowsTask)
		return nil
	}
	fmt.Print(string(out))
	return nil
}

// serviceDoctor reports the Task Scheduler boot-task state + keep-awake for
// `outpost doctor`. Non-destructive — it does NOT launch the task.
func serviceDoctor() []doctorCheck {
	const probe = "$ErrorActionPreference='SilentlyContinue';" +
		"$t=Get-ScheduledTask -TaskName '" + windowsTask + "';" +
		"if($t){'REG=1';'STATE='+$t.State;'TRIG='+$t.Triggers[0].CimClass.CimClassName;" +
		"'RUNAS='+$t.Principal.UserId;'LOGON='+$t.Principal.LogonType}else{'REG=0'};" +
		"'STANDBY='+(((powercfg /query SCHEME_CURRENT SUB_SLEEP STANDBYIDLE)|Select-String 'Current AC')-join '')"
	out, _ := exec.Command(psExe, "-NoProfile", "-Command", probe).Output()
	m := parseKV(string(out))

	var c []doctorCheck
	if m["REG"] != "1" {
		c = append(c, doctorCheck{"boot-service", "warn", "Task Scheduler task '" + windowsTask + "' not registered — `outpost service install`"})
	} else {
		detail := "task '" + windowsTask + "' state=" + m["STATE"] + " runas=" + m["RUNAS"] + " logon=" + m["LOGON"] + " trigger=" + m["TRIG"]
		if strings.Contains(m["TRIG"], "Boot") {
			c = append(c, doctorCheck{"boot-service", "ok", detail + " — starts at boot"})
		} else {
			c = append(c, doctorCheck{"boot-service", "warn", detail + " — NOT a boot trigger; won't survive an unattended reboot"})
		}
		c = append(c, doctorCheck{"will-run", "info", "validate on demand with `Start-ScheduledTask -TaskName " + windowsTask + "` (NOT `schtasks /Run`, which falsely reports 0xFFFFFFFF)"})
	}
	if strings.Contains(m["STANDBY"], "0x00000000") {
		c = append(c, doctorCheck{"keep-awake", "ok", "idle sleep disabled (AC)"})
	} else if m["STANDBY"] != "" {
		c = append(c, doctorCheck{"keep-awake", "warn", "idle sleep enabled — a sleeping host drops the tunnel; `powercfg /change standby-timeout-ac 0`"})
	}
	return c
}

// isWindowsAdmin reports whether the current process holds the Administrators
// role (elevated token). Uses the .NET WindowsPrincipal check — no extra Go
// dependency, matches the install.ps1 elevation probe.
func isWindowsAdmin() bool {
	const cmd = "([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent())" +
		".IsInRole([Security.Principal.WindowsBuiltinRole]::Administrator)"
	out, err := exec.Command(psExe, "-NoProfile", "-Command", cmd).Output()
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(string(out)), "True")
}
