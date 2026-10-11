//go:build !linux && !darwin && !windows

package plugin

import "fmt"

func lockIPAM(string) (func(), error) {
	return nil, fmt.Errorf("ipam: unsupported platform")
}
