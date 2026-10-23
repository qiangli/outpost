//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
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
		"$ErrorActionPreference='Stop'; Start-ScheduledTask -TaskName '"+windowsTask+"'").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// windowsUserID is DOMAIN\User (or just User) — the principal the task runs as.
// windowsUserID names the account the task runs as. The process token is the
// authority: over ssh on a workgroup machine USERDOMAIN can read "WORKGROUP",
// which Task Scheduler cannot map to a SID (HRESULT 0x80070534). The
// environment is only a fallback when the token lookup fails.
func windowsUserID() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
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
		return nil
	}
	if opts.System && !isWindowsAdmin() {
		return fmt.Errorf("system service install needs Administrator — run in an elevated PowerShell:\n  %s", serviceElevatedRetry(self, user))
	}
	if err := preflightTakeover(opts); err != nil {
		return err
	}
	out, err := exec.Command(psExe, "-NoProfile", "-Command", body).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Register-ScheduledTask failed: %w\n%s", err, out)
	}
	// -AtStartup / -AtLogOn won't fire until the next boot/logon — kick the task
	// off now so `service install` is immediately effective (matches the
	// launchd/systemd paths, which start on install via RunAtLoad / --now).
	// Start-ScheduledTask, NOT `schtasks /Run` (see startTask).
	if err2 := startTask(); err2 != nil {
		return fmt.Errorf("task registered but immediate start failed: %w", err2)
	}
	fmt.Printf("registered Task Scheduler task %q — runs `outpost supervisord` %s as %q (started now)\n", windowsTask, kind, user)
	return nil
}

// removeManagedRegistrations stops a daemon this binary manages so the new
// supervisor can claim the singleton pidfile. Both modes use the same task name
// (Register -Force replaces it), but replacing the task does not stop an
// already-running supervisor. Stop the task first so it cannot respawn the
// daemon while the replacement registration is being installed.
func removeManagedRegistrations(_ installOpts) {
	_ = exec.Command(psExe, "-NoProfile", "-NonInteractive", "-Command",
		"$ErrorActionPreference='Stop'; $t=Get-ScheduledTask -TaskName '"+windowsTask+"' -ErrorAction SilentlyContinue; if($t){Stop-ScheduledTask -TaskName '"+windowsTask+"'}").Run()
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
