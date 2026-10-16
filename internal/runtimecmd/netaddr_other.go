//go:build !linux

package runtimecmd

import "fmt"

func addAddress(cidr, name string) error {
	return fmt.Errorf("netaddr add requires Linux")
}
