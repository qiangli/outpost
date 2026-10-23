//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/qiangli/outpost/internal/agent/postinstall"
)

func systemdUserUnitPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "systemd", "user", systemdUnit)
}

func systemdSystemUnitPath() string {
	return filepath.Join("/etc", "systemd", "system", systemdUnit)
}

func installService(opts installOpts) error {
	self, err := serviceTarget()
	if err != nil {
		return err
	}
	if !opts.System {
		return installSystemdUser(self, opts)
	}
	return installSystemdSystem(self, opts)
}

// installSystemdUser — per-user, no admin, starts at login (linger to survive
// logout).
func installSystemdUser(self string, opts installOpts) error {
	unit := renderSystemdUserUnit(self)
	path := systemdUserUnitPath()
	if opts.DryRun {
		fmt.Printf("# (--user) write %s:\n%s\n# systemctl --user daemon-reload\n# systemctl --user enable --now %s\n# loginctl enable-linger\n",
			path, unit, systemdUnit)
		return nil
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl not found — cannot register a systemd --user service; run `outpost supervisord` under your init manager instead")
	}
	if err := preflightTakeover(opts); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir unit dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w", err)
	}
	if err := exec.Command("systemctl", "--user", "enable", "--now", systemdUnit).Run(); err != nil {
		return fmt.Errorf("systemctl --user enable --now failed (unit at %s): %w", path, err)
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	c := postinstall.Command{"loginctl", "--no-ask-password", "enable-linger", u.Username}
	r := postinstall.Exec(context.Background(), c)
	if !r.OK() {
		return fmt.Errorf("enable linger failed: %s %s\nRun:\n  sudo %s", r.Error, r.Value(), postinstall.ShellCommand("linux", c))
	}
	r = postinstall.Exec(context.Background(), postinstall.Command{"loginctl", "show-user", u.Username, "--property=Linger", "--value"})
	if !r.OK() || r.Value() != "yes" {
		return fmt.Errorf("linger readback failed; run: sudo %s", postinstall.ShellCommand("linux", c))
	}
	fmt.Printf("registered systemd --user unit %s with linger — starts at boot\n", systemdUnit)
	return nil
}

// installSystemdSystem — system, admin (root), starts at boot, runs as the user.
func installSystemdSystem(self string, opts installOpts) error {
	runUser, _, err := resolveRunAsUnix(opts.RunAs)
	if err != nil {
		return err
	}
	unit := renderSystemdSystemUnit(self, runUser)
	path := systemdSystemUnitPath()
	if opts.DryRun {
		fmt.Printf("# (system) write %s:\n%s\n# systemctl daemon-reload\n# systemctl enable --now %s\n",
			path, unit, systemdUnit)
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("system service install needs root — run:\n  %s", serviceElevatedRetry(self, runUser))
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl not found — cannot register a systemd system service")
	}
	if err := preflightTakeover(opts); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit %s: %w", path, err)
	}
	if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if err := exec.Command("systemctl", "enable", "--now", systemdUnit).Run(); err != nil {
		return fmt.Errorf("systemctl enable --now failed (unit at %s): %w", path, err)
	}
	fmt.Printf("registered systemd system unit %s — starts `outpost supervisord` at boot as %q (no login required)\n", systemdUnit, runUser)
	return nil
}

func uninstallService(opts installOpts) error {
	if !opts.System {
		_ = exec.Command("systemctl", "--user", "disable", "--now", systemdUnit).Run()
		if err := os.Remove(systemdUserUnitPath()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove unit: %w", err)
		}
		if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err != nil {
			return fmt.Errorf("systemctl --user daemon-reload: %w", err)
		}
		fmt.Printf("unregistered systemd --user unit %s\n", systemdUnit)
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("system service uninstall needs root — re-run with sudo")
	}
	_ = exec.Command("systemctl", "disable", "--now", systemdUnit).Run()
	if err := os.Remove(systemdSystemUnitPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit: %w", err)
	}
	if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	fmt.Printf("unregistered systemd system unit %s\n", systemdUnit)
	return nil
}

// removeManagedRegistrations tears down BOTH systemd registrations this binary
// owns (system unit + --user unit) and stops their daemon, so a fresh install OR
// a re-install cleanly supersedes whatever was there and frees the singleton
// pidfile. Best-effort and quiet; system-scope ops no-op without root.
func removeManagedRegistrations(opts installOpts) {
	// system unit (needs root)
	_ = exec.Command("systemctl", "disable", "--now", systemdUnit).Run()
	_ = os.Remove(systemdSystemUnitPath())
	// --user unit — the run-as user under sudo (via runuser), else current user
	if name, home, err := resolveRunAsUnix(opts.RunAs); err == nil && os.Geteuid() == 0 {
		if u, lookupErr := user.Lookup(name); lookupErr == nil {
			runtimeDir := "/run/user/" + u.Uid
			_ = exec.Command("runuser", "-u", name, "--", "env", "XDG_RUNTIME_DIR="+runtimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path="+runtimeDir+"/bus", "systemctl", "--user", "disable", "--now", systemdUnit).Run()
		}
		_ = os.Remove(filepath.Join(home, ".config", "systemd", "user", systemdUnit))
	} else {
		_ = exec.Command("systemctl", "--user", "disable", "--now", systemdUnit).Run()
		_ = os.Remove(systemdUserUnitPath())
	}
}

func statusService(opts installOpts) error {
	scope := "--user"
	if opts.System {
		scope = "system"
	}
	enabled := systemctlShow(opts.System, "is-enabled")
	active := systemctlShow(opts.System, "is-active")
	fmt.Printf("systemd %s %s: enabled=%s active=%s\n", scope, systemdUnit, enabled, active)
	return nil
}

func systemctlShow(system bool, verb string) string {
	args := []string{"--user", verb, systemdUnit}
	if system {
		args = []string{verb, systemdUnit}
	}
	out, _ := exec.Command("systemctl", args...).Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "unknown"
	}
	return s
}
