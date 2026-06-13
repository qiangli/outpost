//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

func launchdPlistPath() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", launchdLabel+".plist")
}

func installService(dryRun bool) error {
	self, err := serviceTarget()
	if err != nil {
		return err
	}
	plist := renderLaunchdPlist(self, os.Getenv("HOME"))
	path := launchdPlistPath()
	uid := strconv.Itoa(os.Getuid())
	if dryRun {
		fmt.Printf("# write %s:\n%s\n# launchctl bootout gui/%s/%s   (ignore error)\n# launchctl bootstrap gui/%s %s\n",
			path, plist, uid, launchdLabel, uid, path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir LaunchAgents: %w", err)
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	// bootout first so a re-install picks up the new ProgramArguments.
	_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+launchdLabel).Run()
	if err := exec.Command("launchctl", "bootstrap", "gui/"+uid, path).Run(); err != nil {
		return fmt.Errorf("launchctl bootstrap failed (plist at %s — load manually: launchctl bootstrap gui/%s %s): %w", path, uid, path, err)
	}
	fmt.Printf("registered launchd agent %s — runs `outpost supervisord` at login\n", launchdLabel)
	return nil
}

func uninstallService() error {
	uid := strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+launchdLabel).Run()
	if err := os.Remove(launchdPlistPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist: %w", err)
	}
	fmt.Printf("unregistered launchd agent %s\n", launchdLabel)
	return nil
}

func statusService() error {
	uid := strconv.Itoa(os.Getuid())
	if err := exec.Command("launchctl", "print", "gui/"+uid+"/"+launchdLabel).Run(); err == nil {
		fmt.Printf("launchd agent %s: loaded\n", launchdLabel)
		return nil
	}
	if _, err := os.Stat(launchdPlistPath()); err == nil {
		fmt.Printf("launchd agent %s: plist present but not loaded\n", launchdLabel)
	} else {
		fmt.Printf("launchd agent %s: not registered\n", launchdLabel)
	}
	return nil
}
