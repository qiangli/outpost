//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
		return installSystemdUser(self, opts.DryRun)
	}
	return installSystemdSystem(self, opts)
}

// installSystemdUser — per-user, no admin, starts at login (linger to survive
// logout).
func installSystemdUser(self string, dryRun bool) error {
	unit := renderSystemdUserUnit(self)
	path := systemdUserUnitPath()
	if dryRun {
		fmt.Printf("# (--user) write %s:\n%s\n# systemctl --user daemon-reload\n# systemctl --user enable --now %s\n",
			path, unit, systemdUnit)
		return nil
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl not found — cannot register a systemd --user service; run `outpost supervisord` under your init manager instead")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir unit dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	if err := exec.Command("systemctl", "--user", "enable", "--now", systemdUnit).Run(); err != nil {
		return fmt.Errorf("systemctl --user enable --now failed (unit at %s): %w", path, err)
	}
	fmt.Printf("registered systemd --user unit %s — runs `outpost supervisord` at login\n", systemdUnit)
	user := os.Getenv("USER")
	if user == "" {
		user = strconv.Itoa(os.Getuid())
	}
	if _, err := os.Stat("/var/lib/systemd/linger/" + user); err != nil {
		fmt.Printf("note: on a headless box, enable linger so the unit survives logout:\n  sudo loginctl enable-linger %s\n", user)
	}
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
		return fmt.Errorf("system service install needs root — re-run with sudo:\n  sudo %s service install --run-as %s\n(or use --user for a no-admin per-login install)", self, runUser)
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl not found — cannot register a systemd system service")
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit %s: %w", path, err)
	}
	_ = exec.Command("systemctl", "daemon-reload").Run()
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
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
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
	_ = exec.Command("systemctl", "daemon-reload").Run()
	fmt.Printf("unregistered systemd system unit %s\n", systemdUnit)
	return nil
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
