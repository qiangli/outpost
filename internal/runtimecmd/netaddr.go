package runtimecmd

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/spf13/cobra"
)

func netaddrCmd() *cobra.Command { return netaddrCommand(addAddress) }

func netaddrCommand(add func(string, string) error) *cobra.Command {
	cmd := &cobra.Command{Use: "netaddr", Short: "Manage runtime interface addresses"}
	cmd.AddCommand(&cobra.Command{Use: "add CIDR dev INTERFACE", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 3 || args[1] != "dev" {
			return fmt.Errorf("usage: netaddr add CIDR dev INTERFACE")
		}
		_, err := netip.ParsePrefix(args[0])
		return err
	}, RunE: func(_ *cobra.Command, args []string) error { return add(args[0], args[2]) }})
	cmd.AddCommand(&cobra.Command{Use: "exists INTERFACE", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		_, err := net.InterfaceByName(args[0])
		return err
	}})
	cmd.AddCommand(&cobra.Command{Use: "ipv4 INTERFACE", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		iface, err := net.InterfaceByName(args[0])
		if err != nil {
			return err
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return err
		}
		for _, addr := range addrs {
			prefix, err := netip.ParsePrefix(addr.String())
			if err == nil && prefix.Addr().Is4() {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), prefix.Addr())
				return err
			}
		}
		return fmt.Errorf("interface has no IPv4 address")
	}})
	return cmd
}
