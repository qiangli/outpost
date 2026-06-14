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
)

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
		fmt.Printf("# %s -NoProfile -Command \"%s\"\n# %s /Run /TN %s\n", psExe, body, schtasksExe, windowsTask)
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
	// -AtStartup / -AtLogOn won't fire until the next boot/logon — kick the task
	// off now so `service install` is immediately effective (matches the
	// launchd/systemd paths, which start on install via RunAtLoad / --now).
	if out2, err2 := exec.Command(schtasksExe, "/Run", "/TN", windowsTask).CombinedOutput(); err2 != nil {
		fmt.Printf("note: task registered but immediate start failed (it will start %s): %v\n%s\n", kind, err2, out2)
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
