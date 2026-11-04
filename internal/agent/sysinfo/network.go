package sysinfo

import (
	"net"
	"slices"
)

func lanIPs() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var ips []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		ips = append(ips, privateIPs(addrs)...)
	}
	slices.Sort(ips)
	return slices.Compact(ips)
}

func privateIPs(addrs []net.Addr) []string {
	var ips []string
	for _, addr := range addrs {
		var ip net.IP
		switch addr := addr.(type) {
		case *net.IPNet:
			ip = addr.IP
		case *net.IPAddr:
			ip = addr.IP
		}
		if ip.IsPrivate() && !ip.IsLoopback() {
			ips = append(ips, ip.String())
		}
	}
	return ips
}
