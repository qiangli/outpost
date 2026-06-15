//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
)

func launchAgentPath() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", launchdLabel+".plist")
}

func launchDaemonPath() string {
	return filepath.Join("/Library", "LaunchDaemons", launchdLabel+".plist")
}

func installService(opts installOpts) error {
	self, err := serviceTarget()
	if err != nil {
		return err
	}
	if !opts.System {
		return installLaunchAgent(self, opts)
	}
	return installLaunchDaemon(self, opts)
}

// installLaunchAgent — per-user, no admin, starts at login.
func installLaunchAgent(self string, opts installOpts) error {
	plist := renderLaunchAgentPlist(self, os.Getenv("HOME"))
	path := launchAgentPath()
	uid := strconv.Itoa(os.Getuid())
	if opts.DryRun {
		fmt.Printf("# (--user) write %s:\n%s\n# launchctl bootout gui/%s/%s   (ignore error)\n# launchctl bootstrap gui/%s %s\n",
			path, plist, uid, launchdLabel, uid, path)
		return nil
	}
	if err := preflightTakeover(opts); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir LaunchAgents: %w", err)
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+launchdLabel).Run()
	if err := exec.Command("launchctl", "bootstrap", "gui/"+uid, path).Run(); err != nil {
		return fmt.Errorf("launchctl bootstrap failed (plist at %s — load manually: launchctl bootstrap gui/%s %s): %w", path, uid, path, err)
	}
	fmt.Printf("registered launchd agent %s (--user) — runs `outpost supervisord` at login\n", launchdLabel)
	return nil
}

// installLaunchDaemon — system, admin (root), starts at boot, runs as the user.
func installLaunchDaemon(self string, opts installOpts) error {
	runUser, home, err := resolveRunAsUnix(opts.RunAs)
	if err != nil {
		return err
	}
	plist := renderLaunchDaemonPlist(self, runUser, home)
	path := launchDaemonPath()
	if opts.DryRun {
		fmt.Printf("# (system) write %s (root:wheel 0644):\n%s\n# launchctl bootout system/%s   (ignore error)\n# launchctl bootstrap system %s\n",
			path, plist, launchdLabel, path)
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("system service install needs root — re-run with sudo:\n  sudo %s service install --run-as %s\n(or use --user for a no-admin per-login install)", self, runUser)
	}
	if err := preflightTakeover(opts); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write LaunchDaemon plist %s: %w", path, err)
	}
	// root:wheel ownership is what launchd requires for a system daemon.
	_ = os.Chown(path, 0, 0)
	_ = exec.Command("launchctl", "bootout", "system/"+launchdLabel).Run()
	if err := exec.Command("launchctl", "bootstrap", "system", path).Run(); err != nil {
		return fmt.Errorf("launchctl bootstrap system failed (plist at %s — load manually: sudo launchctl bootstrap system %s): %w", path, path, err)
	}
	fmt.Printf("registered launchd daemon %s — starts `outpost supervisord` at boot as %q (no login required)\n", launchdLabel, runUser)
	return nil
}

func uninstallService(opts installOpts) error {
	if !opts.System {
		uid := strconv.Itoa(os.Getuid())
		_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+launchdLabel).Run()
		if err := os.Remove(launchAgentPath()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove plist: %w", err)
		}
		fmt.Printf("unregistered launchd agent %s (--user)\n", launchdLabel)
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("system service uninstall needs root — re-run with sudo")
	}
	_ = exec.Command("launchctl", "bootout", "system/"+launchdLabel).Run()
	if err := os.Remove(launchDaemonPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove LaunchDaemon plist: %w", err)
	}
	fmt.Printf("unregistered launchd daemon %s\n", launchdLabel)
	return nil
}

// serviceDoctor reports the launchd boot-service state for `outpost doctor`.
func serviceDoctor() []doctorCheck {
	if exec.Command("launchctl", "print", "system/"+launchdLabel).Run() == nil {
		return []doctorCheck{{"boot-service", "ok", "launchd system daemon " + launchdLabel + " loaded — starts at boot"}}
	}
	if _, err := os.Stat(launchDaemonPath()); err == nil {
		return []doctorCheck{{"boot-service", "warn", "system plist present but not loaded: " + launchDaemonPath() + " — `sudo launchctl bootstrap system " + launchDaemonPath() + "`"}}
	}
	uid := strconv.Itoa(os.Getuid())
	if exec.Command("launchctl", "print", "gui/"+uid+"/"+launchdLabel).Run() == nil {
		return []doctorCheck{{"boot-service", "warn", "only the --user LaunchAgent is loaded — starts at LOGIN, not boot; `sudo outpost service install` for boot persistence"}}
	}
	return []doctorCheck{{"boot-service", "warn", "no launchd registration — `sudo outpost service install`"}}
}

// removeManagedRegistrations tears down BOTH launchd registrations this binary
// owns (system LaunchDaemon + per-user LaunchAgent), so a fresh install OR a
// re-install cleanly supersedes whatever was there. launchctl bootout also kills
// the job's running process, freeing the singleton pidfile for the new
// supervisor. Best-effort and quiet; system-domain ops no-op without root.
func removeManagedRegistrations(opts installOpts) {
	// system LaunchDaemon (needs root)
	_ = exec.Command("launchctl", "bootout", "system/"+launchdLabel).Run()
	_ = os.Remove(launchDaemonPath())
	// per-user LaunchAgent — the run-as user under sudo, else the current user
	if name, home, err := resolveRunAsUnix(opts.RunAs); err == nil {
		if u, e := user.Lookup(name); e == nil {
			_ = exec.Command("launchctl", "bootout", "gui/"+u.Uid+"/"+launchdLabel).Run()
		}
		_ = os.Remove(filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"))
	} else {
		uid := strconv.Itoa(os.Getuid())
		_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+launchdLabel).Run()
		_ = os.Remove(launchAgentPath())
	}
}

func statusService(opts installOpts) error {
	if !opts.System {
		uid := strconv.Itoa(os.Getuid())
		if err := exec.Command("launchctl", "print", "gui/"+uid+"/"+launchdLabel).Run(); err == nil {
			fmt.Printf("launchd agent %s (--user): loaded\n", launchdLabel)
			return nil
		}
		if _, err := os.Stat(launchAgentPath()); err == nil {
			fmt.Printf("launchd agent %s (--user): plist present but not loaded\n", launchdLabel)
		} else {
			fmt.Printf("launchd agent %s (--user): not registered\n", launchdLabel)
		}
		return nil
	}
	if err := exec.Command("launchctl", "print", "system/"+launchdLabel).Run(); err == nil {
		fmt.Printf("launchd daemon %s: loaded\n", launchdLabel)
		return nil
	}
	if _, err := os.Stat(launchDaemonPath()); err == nil {
		fmt.Printf("launchd daemon %s: plist present but not loaded\n", launchdLabel)
	} else {
		fmt.Printf("launchd daemon %s: not registered\n", launchdLabel)
	}
	return nil
}
