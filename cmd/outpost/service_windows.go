//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

const (
	psExe       = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	schtasksExe = `C:\Windows\System32\schtasks.exe`
)

// windowsUserID is DOMAIN\User (or just User) — the principal the logon task
// runs as. Matches install.ps1.
func windowsUserID() string {
	if d := os.Getenv("USERDOMAIN"); d != "" {
		return d + `\` + os.Getenv("USERNAME")
	}
	return os.Getenv("USERNAME")
}

func installService(dryRun bool) error {
	self, err := serviceTarget()
	if err != nil {
		return err
	}
	body := renderWindowsRegisterCmd(self, windowsUserID())
	if dryRun {
		fmt.Printf("# %s -NoProfile -Command \"%s\"\n", psExe, body)
		return nil
	}
	out, err := exec.Command(psExe, "-NoProfile", "-Command", body).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Register-ScheduledTask failed: %w\n%s", err, out)
	}
	fmt.Printf("registered Task Scheduler task %q — runs `outpost supervisord` at logon\n", windowsTask)
	return nil
}

func uninstallService() error {
	out, err := exec.Command(schtasksExe, "/Delete", "/TN", windowsTask, "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks /Delete failed: %w\n%s", err, out)
	}
	fmt.Printf("unregistered Task Scheduler task %q\n", windowsTask)
	return nil
}

func statusService() error {
	out, err := exec.Command(schtasksExe, "/Query", "/TN", windowsTask).CombinedOutput()
	if err != nil {
		fmt.Printf("Task Scheduler task %q: not registered\n", windowsTask)
		return nil
	}
	fmt.Print(string(out))
	return nil
}
