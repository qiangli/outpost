package mesh

import (
	"testing"

	ma "github.com/multiformats/go-multiaddr"
)

func TestClassifyConnAddr(t *testing.T) {
	cases := []struct{ addr, want string }{
		{"/ip4/10.0.0.5/tcp/4001", "lan"},            // RFC-1918 wifi LAN
		{"/ip4/192.168.1.9/udp/4001/quic-v1", "lan"}, // RFC-1918 LAN
		{"/ip4/172.16.4.4/tcp/4001", "lan"},          // RFC-1918 LAN
		{"/ip4/169.254.110.47/tcp/4001", "tp"},       // APIPA / link-local = direct wired (TP-Link)
		{"/ip6/fe80::1/tcp/4001", "tp"},              // IPv6 link-local
		{"/ip4/76.103.216.225/tcp/16690", "wan"},     // public
		{"/ip4/127.0.0.1/tcp/4001", ""},              // loopback ignored
	}
	for _, c := range cases {
		m, err := ma.NewMultiaddr(c.addr)
		if err != nil {
			t.Fatalf("%s: %v", c.addr, err)
		}
		if got := classifyConnAddr(m); got != c.want {
			t.Errorf("%s → %q, want %q", c.addr, got, c.want)
		}
	}
}

func TestStrongerLinkClass(t *testing.T) {
	// tp > lan > wan > ""
	if strongerLinkClass("lan", "tp") != "tp" {
		t.Error("tp should beat lan")
	}
	if strongerLinkClass("wan", "lan") != "lan" {
		t.Error("lan should beat wan")
	}
	if strongerLinkClass("", "wan") != "wan" {
		t.Error("wan should beat empty")
	}
	if strongerLinkClass("tp", "lan") != "tp" {
		t.Error("tp should stay over lan")
	}
}
