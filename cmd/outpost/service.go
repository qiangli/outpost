package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// service.go is the cross-platform `outpost service` command: register the
// always-up supervisor (`outpost supervisord`) with the OS init system so it —
// and through it the daemon — survive a reboot and restart on failure.
//
// Two modes:
//   - SYSTEM (default): a privileged install (sudo / UAC) that registers a
//     system service starting at BOOT with NO login required, then drops to and
//     runs as the (regular, non-elevated) target user. Identical behavior on all
//     three platforms — systemd system unit `User=`, launchd LaunchDaemon
//     `UserName`, Windows Task Scheduler `-AtStartup -LogonType S4U -RunLevel
//     Limited`.
//   - USER (`--user`): the no-admin fallback — per-login-session registration
//     (systemd --user / launchd LaunchAgent / Task Scheduler `-AtLogOn`). Starts
//     when the user logs in, NOT at boot. For hosts where admin isn't available.
//
// The per-platform install/uninstall/status live in service_{darwin,linux,
// windows}.go; the pure render helpers below are shared + unit-tested.

// Identifiers for the registered service on each platform.
const (
	launchdLabel = "io.dhnt.outpost" // macOS launchd label (Agent + Daemon)
	systemdUnit  = "outpost.service" // Linux systemd unit (--user + system)
	windowsTask  = "outpost"         // Windows Task Scheduler task name
)

// installOpts carries the resolved install mode down into the platform code.
type installOpts struct {
	System bool   // true = boot-time system service running as RunAs; false = per-user
	DryRun bool   // print the definition + commands, apply nothing
	Force  bool   // install even if a daemon under a foreign launcher is still running
	RunAs  string // OS user the system service runs as ("" = invoking non-root user)
}

func serviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Register the outpost supervisor to start at boot and stay up (launchd/systemd/Task Scheduler)",
	}
	cmd.AddCommand(serviceInstallCmd(), serviceUninstallCmd(), serviceStatusCmd())
	return cmd
}

func serviceInstallCmd() *cobra.Command {
	var dryRun, userMode, force bool
	var runAs string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Register `outpost supervisord` to start at boot as the target user (restart on failure)",
		Long: `Register the outpost supervisor with the OS init system.

Default (system service): starts at BOOT with no login required, running as the
regular (non-elevated) target user. Requires admin at install time — re-run with
sudo (macOS/Linux) or from an elevated prompt (Windows).

--user: no-admin fallback. Registers under your login session; starts when you
log in, not at boot.

If an outpost daemon is already running, install takes over a prior registration
made by this binary; if a daemon survives under a launcher it can't manage (a
manual ` + "`outpost start`" + `, an ` + "`outpost run`" + ` job, etc.) it refuses rather than
create two managers fighting over the pidfile — pass --force to override.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return installService(installOpts{System: !userMode, DryRun: dryRun, Force: force, RunAs: runAs})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the service definition + the commands that would run, without applying")
	cmd.Flags().BoolVar(&userMode, "user", false, "Per-user mode (no admin): start at login instead of boot. Default is a system service that starts at boot.")
	cmd.Flags().BoolVar(&force, "force", false, "Install even if a daemon under a launcher this command can't manage is still running")
	cmd.Flags().StringVar(&runAs, "run-as", "", "OS user the system service runs as (default: the invoking non-root user)")
	return cmd
}

func serviceUninstallCmd() *cobra.Command {
	var userMode bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Unregister the outpost supervisor service",
		RunE: func(_ *cobra.Command, _ []string) error {
			return uninstallService(installOpts{System: !userMode})
		},
	}
	cmd.Flags().BoolVar(&userMode, "user", false, "Uninstall the per-user registration instead of the system service")
	return cmd
}

func serviceStatusCmd() *cobra.Command {
	var userMode bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether the outpost supervisor service is registered + running",
		RunE: func(_ *cobra.Command, _ []string) error {
			return statusService(installOpts{System: !userMode})
		},
	}
	cmd.Flags().BoolVar(&userMode, "user", false, "Report the per-user registration instead of the system service")
	return cmd
}

// serviceTarget is the executable the service launches, as `<self> supervisord`.
func serviceTarget() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate executable: %w", err)
	}
	return self, nil
}

// daemonAdminAddr is the loopback admin endpoint a running daemon always binds
// (`outpost start` brings it up unconditionally). Probing it is a launcher-
// agnostic "is a daemon already running here?" check.
func daemonAdminAddr() string {
	if a := os.Getenv("OUTPOST_ADMIN_ADDR"); a != "" {
		return a
	}
	return "127.0.0.1:17777"
}

func daemonRunning() bool {
	c, err := net.DialTimeout("tcp", daemonAdminAddr(), 600*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// preflightTakeover makes the service being installed the SOLE daemon manager.
// A second manager (a leftover --user agent, a manual `outpost start`, an
// `outpost run` launchd/systemd job, …) would race the supervisor for outpost's
// singleton pidfile and crash-loop it. So before bootstrapping:
//
//  1. if no daemon is running, there's nothing to reconcile;
//  2. otherwise remove THIS binary's other-mode registration — if it was the
//     manager, that stops the daemon and frees the port;
//  3. if a daemon still survives, it's under a launcher we can't safely
//     identify/disable — refuse with guidance (or proceed under --force).
func preflightTakeover(opts installOpts) error {
	if !daemonRunning() {
		return nil
	}
	removeManagedRegistrations(opts) // best-effort, quiet — stops a daemon WE manage
	time.Sleep(1500 * time.Millisecond)
	if !daemonRunning() {
		fmt.Println("note: replaced a prior outpost registration that this command manages")
		return nil
	}
	if opts.Force {
		fmt.Println("warning: an outpost daemon is still running under another launcher — proceeding due to --force.\n         You MUST stop that launcher, or the supervisor will conflict on the pidfile.")
		return nil
	}
	return fmt.Errorf("an outpost daemon is already running (admin %s responding) under a launcher this command doesn't manage —\n"+
		"a manual `outpost start`, an `outpost run` job, or a hand-written init entry.\n"+
		"Stop it and disable that launcher first, then re-run; or pass --force to install anyway.\n"+
		"  macOS:   launchctl list | grep -i outpost\n"+
		"  Linux:   systemctl --user list-units 'outpost*'; systemctl list-units 'outpost*'\n"+
		"  Windows: schtasks /Query | findstr /i outpost", daemonAdminAddr())
}

// ---- per-user (no-admin) renders — start at LOGIN -------------------------

// renderLaunchAgentPlist is the macOS LaunchAgent plist (per-user, starts at
// login, no admin) running `<self> supervisord`. Mirrors the old install.sh
// register_launchd path.
func renderLaunchAgentPlist(self, home string) string {
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

// renderSystemdUserUnit is the Linux systemd --user unit (per-user, no admin)
// running `<self> supervisord`.
func renderSystemdUserUnit(self string) string {
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

// renderWindowsLogonTask is the per-user Task Scheduler registration (no admin):
// runs `<self> supervisord` at the user's interactive logon.
//
// LogonType is `Interactive`, NOT `InteractiveToken`: the latter is the COM
// API / Task Scheduler XML spelling, but the New-ScheduledTaskPrincipal cmdlet
// enum only accepts None/Password/S4U/Interactive/Group/ServiceAccount/
// InteractiveOrPassword. `Interactive` maps to the same
// TASK_LOGON_INTERACTIVE_TOKEN — run only while the user is logged on.
func renderWindowsLogonTask(self, userID string) string {
	return fmt.Sprintf(
		"$a = New-ScheduledTaskAction -Execute '%s' -Argument 'supervisord'; "+
			"$t = New-ScheduledTaskTrigger -AtLogOn -User '%s'; "+
			"$p = New-ScheduledTaskPrincipal -UserId '%s' -LogonType Interactive -RunLevel Limited; "+
			"Register-ScheduledTask -TaskName '%s' -Action $a -Trigger $t -Principal $p -Force",
		self, userID, userID, windowsTask)
}

// ---- system (admin) renders — start at BOOT, run as the target user -------

// renderLaunchDaemonPlist is the macOS LaunchDaemon plist (system, admin):
// launchd starts `<self> supervisord` at boot and runs it as UserName, with no
// login required. HOME is set explicitly because a LaunchDaemon does not inherit
// the target user's login environment, and outpost resolves its config/cache
// dirs from $HOME.
func renderLaunchDaemonPlist(self, user, home string) string {
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
    <key>UserName</key><string>%s</string>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>ThrottleInterval</key><integer>10</integer>
    <key>WorkingDirectory</key><string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key><string>%s</string>
    </dict>
</dict>
</plist>
`, launchdLabel, self, user, home, home)
}

// renderSystemdSystemUnit is the Linux systemd system unit (admin): starts
// `<self> supervisord` at boot under `User=`, with no login required.
// multi-user.target is reached at boot before any login.
func renderSystemdSystemUnit(self, user string) string {
	return fmt.Sprintf(`[Unit]
Description=outpost — home-host agent supervisor for ai.dhnt.io
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
ExecStart=%s supervisord
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
`, user, self)
}

// renderWindowsStartupTask is the system Task Scheduler registration (admin):
// runs `<self> supervisord` at BOOT (-AtStartup) as the target user with no
// login required.
//
//   - -LogonType S4U: run as the user WITHOUT storing a password (service-for-
//     user). Outbound sockets (the matrix tunnel) and the user's local profile
//     work; the only thing S4U can't do is reach SMB shares as the user, which
//     outpost never does.
//   - -RunLevel Limited: the daemon runs with the user's STANDARD token — a
//     regular, non-elevated user, even though an admin registered the task.
//   - settings: no execution time limit (it's a daemon), survive battery, and a
//     task-level restart backstop complementing the supervisord's own restart.
func renderWindowsStartupTask(self, userID string) string {
	return fmt.Sprintf(
		"$a = New-ScheduledTaskAction -Execute '%s' -Argument 'supervisord'; "+
			"$t = New-ScheduledTaskTrigger -AtStartup; "+
			"$p = New-ScheduledTaskPrincipal -UserId '%s' -LogonType S4U -RunLevel Limited; "+
			"$s = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries "+
			"-ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1); "+
			"Register-ScheduledTask -TaskName '%s' -Action $a -Trigger $t -Principal $p -Settings $s -Force",
		self, userID, windowsTask)
}
