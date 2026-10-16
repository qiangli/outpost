//go:build linux

package runtimecmd

import (
	"errors"
	"syscall"

	"github.com/vishvananda/netlink"
)

func addAddress(cidr, name string) error {
	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		return err
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	err = netlink.AddrAdd(link, addr)
	if errors.Is(err, syscall.EEXIST) {
		return nil
	}
	return err
}
