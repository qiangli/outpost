//go:build linux

package machineid

import (
	"errors"
	"os"
	"strings"
)

var linuxMachineIDPaths = []string{
	"/etc/machine-id",
	"/var/lib/dbus/machine-id",
}

// osMachineID reads the Linux machine ID from /etc/machine-id,
// falling back to /var/lib/dbus/machine-id.
func osMachineID() (string, error) {
	for _, p := range linuxMachineIDPaths {
		data, err := os.ReadFile(p)
		if err == nil {
			s := strings.TrimSpace(string(data))
			if s != "" {
				return s, nil
			}
		}
	}
	return "", errors.New("no linux machine-id found")
}
