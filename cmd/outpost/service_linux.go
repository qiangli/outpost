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

func systemdUnitPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "systemd", "user", systemdUnit)
}

func installService(dryRun bool) error {
	self, err := serviceTarget()
	if err != nil {
		return err
	}
	unit := renderSystemdUnit(self)
	path := systemdUnitPath()
	if dryRun {
		fmt.Printf("# write %s:\n%s\n# systemctl --user daemon-reload\n# systemctl --user enable --now %s\n",
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
	fmt.Printf("registered systemd --user unit %s — runs `outpost supervisord`\n", systemdUnit)
	user := os.Getenv("USER")
	if user == "" {
		user = strconv.Itoa(os.Getuid())
	}
	if _, err := os.Stat("/var/lib/systemd/linger/" + user); err != nil {
		fmt.Printf("note: on a headless box, enable linger so the unit survives logout:\n  sudo loginctl enable-linger %s\n", user)
	}
	return nil
}

func uninstallService() error {
	_ = exec.Command("systemctl", "--user", "disable", "--now", systemdUnit).Run()
	if err := os.Remove(systemdUnitPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit: %w", err)
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Printf("unregistered systemd --user unit %s\n", systemdUnit)
	return nil
}

func statusService() error {
	enabled := systemctlShow("is-enabled")
	active := systemctlShow("is-active")
	fmt.Printf("systemd --user %s: enabled=%s active=%s\n", systemdUnit, enabled, active)
	return nil
}

func systemctlShow(verb string) string {
	out, _ := exec.Command("systemctl", "--user", verb, systemdUnit).Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "unknown"
	}
	return s
}
