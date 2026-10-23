package main

import (
	"encoding/xml"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/postinstall"
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
//   - USER (`--user`): Linux enables linger for boot persistence; macOS and
//     Windows register a session service requiring login. Power policy still
//     requires admin rights unless --keep-sleep is specified.
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

// userServiceManager is the small policy boundary used by pairing.  The
// platform implementations remain responsible for launchd, systemd, and Task
// Scheduler; pairing only needs to turn the per-user service on or off.
// Keeping this boundary here lets command tests prove the pairing policy
// without registering anything on the host running the tests.
type userServiceManager interface {
	Install() error
	Uninstall() error
}

type platformUserServiceManager struct{}

func (platformUserServiceManager) Install() error {
	return installService(installOpts{System: false})
}

func (platformUserServiceManager) Uninstall() error {
	return uninstallService(installOpts{System: false})
}

var pairingServiceManager userServiceManager = platformUserServiceManager{}

func activatePairedService(daemonWasRunning, noService bool) error {
	if daemonWasRunning || noService {
		return nil
	}
	return pairingServiceManager.Install()
}

func deactivatePairedService() error { return pairingServiceManager.Uninstall() }

func serviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Register the outpost supervisor to start at boot and stay up (launchd/systemd/Task Scheduler)",
	}
	cmd.AddCommand(serviceInstallCmd(), serviceUninstallCmd(), serviceStatusCmd())
	return cmd
}

func serviceInstallCmd() *cobra.Command {
	return newServiceInstallCmd(postinstall.Exec, installService)
}

func newServiceInstallCmd(run postinstall.Runner, install func(installOpts) error) *cobra.Command {
	var dryRun, userMode, systemMode, force, keepSleep bool
	var runAs string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Register `outpost supervisord` to start at boot as the target user (restart on failure)",
		Long: `Register the outpost supervisor with the OS init system.

Default (system service): starts at BOOT with no login required, running as the
regular (non-elevated) target user. Requires admin at install time — re-run with
sudo (macOS/Linux) or from an elevated prompt (Windows).

--user: registers a per-user service. Linux enables linger; macOS and Windows
require a login. --keep-sleep avoids the privileged power step.

Sleep is disabled by default on AC and battery power. --keep-sleep preserves
existing power policy. Power changes require administrator rights and failures
are returned with the exact command to run.

macOS uses a LaunchDaemon by default. Login Keychain credentials are unavailable
before login; configure unattended credentials for agents that need them.
The explicit --user fallback uses a LaunchAgent and requires login.
Linux --user enables linger so the user unit starts at boot without login.

If an outpost daemon is already running, install takes over a prior registration
made by this binary; if a daemon survives under a launcher it can't manage (a
manual ` + "`outpost start`" + `, an ` + "`outpost run`" + ` job, etc.) it refuses rather than
create two managers fighting over the pidfile — pass --force to override.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if userMode && systemMode {
				return fmt.Errorf("--user and --system are mutually exclusive")
			}
			if !keepSleep {
				if err := postinstall.ApplyPower(cmd.Context(), runtimeOS, run, dryRun, cmd.OutOrStdout()); err != nil {
					return err
				}
			}
			return install(installOpts{System: !userMode, DryRun: dryRun, Force: force, RunAs: runAs})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the service definition + the commands that would run, without applying")
	cmd.Flags().BoolVar(&userMode, "user", false, "Per-user service: Linux enables linger; macOS/Windows start at login. Power policy still needs admin unless --keep-sleep.")
	cmd.Flags().BoolVar(&systemMode, "system", false, "Register a boot service (default on every OS)")
	cmd.Flags().BoolVar(&keepSleep, "keep-sleep", false, "Preserve existing sleep settings")
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
	var userMode, verbose bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether the outpost supervisor service is registered + running",
		RunE: func(_ *cobra.Command, _ []string) error {
			if verbose {
				runDoctor() // full boot-readiness diagnostic
				return nil
			}
			return statusService(installOpts{System: !userMode})
		},
	}
	cmd.Flags().BoolVar(&userMode, "user", false, "Report the per-user registration instead of the system service")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Full boot-readiness diagnostic (same as `outpost doctor`)")
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
//  1. remove THIS binary's registrations, including dormant login services that
//     would otherwise start a second supervisor at the next login;
//  2. if a managed daemon was running, allow its shutdown to free the port;
//  3. if a daemon still survives, it's under a launcher we can't safely
//     identify/disable — refuse with guidance (or proceed under --force).
func preflightTakeover(opts installOpts) error {
	return reconcileServiceRegistration(opts, daemonRunning, removeManagedRegistrations, time.Sleep)
}

func reconcileServiceRegistration(opts installOpts, running func() bool, remove func(installOpts), wait func(time.Duration)) error {
	wasRunning := running()
	remove(opts) // also remove dormant prior registrations
	if !wasRunning {
		return nil
	}
	wait(1500 * time.Millisecond)
	if !running() {
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
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key><string>%s</string>
    </dict>
</dict>
</plist>
`, launchdLabel, plistString(self), plistString(home), servicePATH)
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
Environment=PATH=%s
ExecStart=%s supervisord
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`, servicePATH, systemdExecPath(self))
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
		"$ErrorActionPreference = 'Stop'; $a = New-ScheduledTaskAction -Execute '%s' -Argument 'supervisord'; "+
			"$t = New-ScheduledTaskTrigger -AtLogOn -User '%s'; "+
			"$p = New-ScheduledTaskPrincipal -UserId '%s' -LogonType Interactive -RunLevel Limited; "+
			"Register-ScheduledTask -TaskName '%s' -Action $a -Trigger $t -Principal $p -Force",
		psString(self), psString(userID), psString(userID), windowsTask)
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
        <key>PATH</key><string>%s</string>
    </dict>
</dict>
</plist>
`, launchdLabel, plistString(self), plistString(user), plistString(home), plistString(home), servicePATH)
}

// servicePATH is the PATH given to the supervised daemon. launchd hands a
// daemon only /usr/bin:/bin:/usr/sbin:/sbin, which excludes every common
// package-manager prefix — so a Homebrew or bashy-installed `podman` is
// invisible and the DKS agent runtime fails with "no podman or docker binary
// on PATH" even though `which podman` resolves fine in a shell. The
// hand-rolled plists this template replaced all carried a PATH for exactly
// this reason; regenerating one without it is a silent regression.
//
// Homebrew prefixes cover both Apple Silicon (/opt/homebrew) and Intel
// (/usr/local); ~/bin is where `outpost bashy --install` and similar drop
// tools. Kept literal rather than inheriting the installing shell's PATH so
// the generated definition is reproducible.
const servicePATH = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin"

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
Environment=PATH=%s
ExecStart=%s supervisord
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
`, user, servicePATH, systemdExecPath(self))
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
		"$ErrorActionPreference = 'Stop'; $a = New-ScheduledTaskAction -Execute '%s' -Argument 'supervisord'; "+
			"$t = New-ScheduledTaskTrigger -AtStartup; "+
			"$p = New-ScheduledTaskPrincipal -UserId '%s' -LogonType S4U -RunLevel Limited; "+
			"$s = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries "+
			"-ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1); "+
			"Register-ScheduledTask -TaskName '%s' -Action $a -Trigger $t -Principal $p -Settings $s -Force",
		psString(self), psString(userID), windowsTask)
}

func plistString(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func psString(s string) string { return strings.ReplaceAll(s, "'", "''") }

func systemdExecPath(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "%", "%%"), "$", "$$")
	if strings.ContainsAny(s, " \t\n\r\\\"") {
		return strconv.Quote(s)
	}
	return s
}
func serviceElevatedRetry(self, runAs string) string {
	c := postinstall.Command{self, "service", "install", "--run-as", runAs, "--keep-sleep"}
	line := postinstall.ShellCommand(runtimeOS, c)
	if runtimeOS != "windows" {
		return "sudo " + line
	}
	return line
}
