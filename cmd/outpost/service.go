package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// service.go is the cross-platform `outpost service` command: register the
// always-up supervisor (`outpost supervisord`) with the OS init system so it —
// and through it the daemon — survive a reboot and restart on failure. The
// per-platform install/uninstall/status live in service_{darwin,linux,windows}.go;
// the pure render helpers below are shared + unit-tested. The launchd/systemd/
// Task-Scheduler shapes mirror the one-shot installers
// (scripts/install.{sh,ps1}), which now DRY down to `outpost service install`.

// Identifiers for the registered service on each platform.
const (
	launchdLabel = "io.dhnt.outpost" // macOS LaunchAgent label
	systemdUnit  = "outpost.service" // Linux systemd --user unit
	windowsTask  = "outpost"         // Windows Task Scheduler task name
)

func serviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Register the outpost supervisor to start at boot/login and stay up (launchd/systemd/Task Scheduler)",
	}
	cmd.AddCommand(serviceInstallCmd(), serviceUninstallCmd(), serviceStatusCmd())
	return cmd
}

func serviceInstallCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Register `outpost supervisord` with the OS init system (start at boot/login, restart on failure)",
		RunE: func(_ *cobra.Command, _ []string) error {
			return installService(dryRun)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the service definition + the commands that would run, without applying")
	return cmd
}

func serviceUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Unregister the outpost supervisor service",
		RunE: func(_ *cobra.Command, _ []string) error {
			return uninstallService()
		},
	}
}

func serviceStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether the outpost supervisor service is registered + running",
		RunE: func(_ *cobra.Command, _ []string) error {
			return statusService()
		},
	}
}

// serviceTarget is the executable the service launches, as `<self> supervisord`.
func serviceTarget() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate executable: %w", err)
	}
	return self, nil
}

// renderLaunchdPlist returns the macOS LaunchAgent plist that runs
// `<self> supervisord` at login and keeps it up. Mirrors install.sh
// register_launchd (RunAtLoad + KeepAlive + ThrottleInterval).
func renderLaunchdPlist(self, home string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>supervisord</string>
    </array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>ThrottleInterval</key><integer>10</integer>
    <key>WorkingDirectory</key><string>%s</string>
</dict>
</plist>
`, launchdLabel, self, home)
}

// renderSystemdUnit returns the Linux systemd --user unit that runs
// `<self> supervisord` and restarts it on failure. Mirrors install.sh
// register_systemd_user.
func renderSystemdUnit(self string) string {
	return fmt.Sprintf(`[Unit]
Description=outpost — home-host agent supervisor for ai.dhnt.io
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s supervisord
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`, self)
}

// renderWindowsRegisterCmd returns the PowerShell -Command body that registers
// the Task Scheduler entry running `<self> supervisord` at logon. Mirrors
// install.ps1 (Register-ScheduledTask cmdlets — space-safe, no admin).
//
// LogonType is `Interactive`, NOT `InteractiveToken`: the latter is the COM
// API / Task Scheduler XML spelling, but the New-ScheduledTaskPrincipal cmdlet
// enum (Microsoft.PowerShell.Cmdletization.GeneratedTypes.ScheduledTask.
// LogonTypeEnum) only accepts None/Password/S4U/Interactive/Group/
// ServiceAccount/InteractiveOrPassword. `Interactive` maps to the same
// TASK_LOGON_INTERACTIVE_TOKEN — run only while the user is logged on, no
// stored password, no admin.
func renderWindowsRegisterCmd(self, userID string) string {
	return fmt.Sprintf(
		"$a = New-ScheduledTaskAction -Execute '%s' -Argument 'supervisord'; "+
			"$t = New-ScheduledTaskTrigger -AtLogOn -User '%s'; "+
			"$p = New-ScheduledTaskPrincipal -UserId '%s' -LogonType Interactive -RunLevel Limited; "+
			"Register-ScheduledTask -TaskName '%s' -Action $a -Trigger $t -Principal $p -Force",
		self, userID, userID, windowsTask)
}
