//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"os/user"
)

// resolveRunAsUnix picks the regular user the system service runs as: the
// explicit --run-as, else SUDO_USER (the human who ran `sudo`), and looks up
// their home dir. Refuses to default to root — a system service that re-runs
// the daemon as root would break outpost's per-user auth/config model.
func resolveRunAsUnix(runAs string) (name, home string, err error) {
	name = runAs
	if name == "" {
		name = os.Getenv("SUDO_USER")
	}
	if name == "" || name == "root" {
		return "", "", fmt.Errorf("cannot determine the regular user to run as — pass --run-as <user> (running the daemon as root is refused)")
	}
	u, err := user.Lookup(name)
	if err != nil {
		return "", "", fmt.Errorf("look up user %q: %w", name, err)
	}
	return u.Username, u.HomeDir, nil
}
