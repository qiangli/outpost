//go:build linux

package ollama

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// PowerOffHost powers the host off. The worker instance is launched with
// shutdown behavior = stop, so this STOPS it (never terminates it).
//
// The system's own poweroff goes first — it stops services and unmounts
// cleanly. If that is unavailable or refused, sync and fall back to the
// reboot(2) power-off, which needs CAP_SYS_BOOT. Both failing is returned
// so the watchdog logs it loudly and retries.
func PowerOffHost() error {
	var errs []error
	for _, argv := range [][]string{{"systemctl", "poweroff"}, {"poweroff"}} {
		bin, err := exec.LookPath(argv[0])
		if err != nil {
			continue
		}
		out, err := exec.Command(bin, argv[1:]...).CombinedOutput()
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("%s: %v: %s", argv[0], err, out))
	}
	syscall.Sync()
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		errs = append(errs, fmt.Errorf("reboot(POWER_OFF): %w", err))
	}
	return errors.Join(errs...)
}
