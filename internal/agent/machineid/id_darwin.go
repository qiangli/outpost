//go:build darwin

package machineid

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var ioregUUIDRegex = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([^"]+)"`)

// osMachineID reads the hardware IOPlatformUUID on macOS using ioreg.
func osMachineID() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmdName := "/usr/sbin/ioreg"
	if _, err := exec.LookPath(cmdName); err != nil {
		cmdName = "ioreg"
	}

	out, err := exec.CommandContext(ctx, cmdName, "-rd1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		return "", err
	}

	match := ioregUUIDRegex.FindSubmatch(out)
	if len(match) < 2 {
		return "", errors.New("IOPlatformUUID not found in ioreg output")
	}

	val := strings.TrimSpace(string(match[1]))
	if val == "" {
		return "", errors.New("empty IOPlatformUUID")
	}
	return val, nil
}
