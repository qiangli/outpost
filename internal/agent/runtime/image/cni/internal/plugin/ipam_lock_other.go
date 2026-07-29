//go:build !linux && !darwin

package plugin

import "fmt"

func lockIPAM(string) (func(), error) {
	return nil, fmt.Errorf("ipam: unsupported platform")
}
