package ollama

import (
	"fmt"
	"net"
)

// primaryLANIPv4 returns this host's primary private (RFC-1918) IPv4
// address — the address a same-LAN peer can reach it on — or "" when none
// is found (host has no LAN IPv4, or only public / link-local ones). It
// enumerates the machine's interface addresses and returns the first
// non-loopback IPv4 in 10/8, 172.16/12, or 192.168/16.
func primaryLANIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	return firstPrivateIPv4(addrs)
}

// firstPrivateIPv4 is the testable core of primaryLANIPv4 — it takes an
// explicit address list so unit tests can feed fake addrs. It returns the
// first non-loopback RFC-1918 IPv4 (dotted-quad) string, or "".
func firstPrivateIPv4(addrs []net.Addr) string {
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		default:
			continue
		}
		v4 := ip.To4()
		if v4 == nil || v4.IsLoopback() {
			continue
		}
		if isRFC1918(v4) {
			return v4.String()
		}
	}
	return ""
}

// isRFC1918 reports whether v4 (a 4-byte IPv4) is in a private range:
// 10.0.0.0/8, 172.16.0.0/12, or 192.168.0.0/16.
func isRFC1918(v4 net.IP) bool {
	switch {
	case v4[0] == 10:
		return true
	case v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31:
		return true
	case v4[0] == 192 && v4[1] == 168:
		return true
	}
	return false
}

// LANEndpoint builds the direct same-LAN inference URL to advertise in the
// registry push — "http://<primary-private-LAN-IPv4>:<port>/v1" — or ""
// when no private LAN IPv4 is available (in which case the caller leaves
// RegistryPushPayload.LANEndpoint empty and cloudbox advertises nothing).
func LANEndpoint(port int) string {
	ip := primaryLANIPv4()
	if ip == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d/v1", ip, port)
}
