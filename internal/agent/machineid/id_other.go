//go:build !darwin && !linux && !windows

package machineid

import "errors"

// osMachineID returns an error on unsupported platforms, which causes
// the machineid package to fall back to the persisted UUID.
func osMachineID() (string, error) {
	return "", errors.New("os machine identifier not supported on this platform")
}
