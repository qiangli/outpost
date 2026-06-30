package mesh

import (
	"testing"

	"github.com/libp2p/go-libp2p/core/network"
	ma "github.com/multiformats/go-multiaddr"
)

// fakeConn is a minimal connStat for exercising isRelayed without a live
// libp2p connection.
type fakeConn struct {
	limited bool
	remote  ma.Multiaddr
}

func (c fakeConn) Stat() network.ConnStats {
	return network.ConnStats{Stats: network.Stats{Limited: c.limited}}
}
func (c fakeConn) RemoteMultiaddr() ma.Multiaddr { return c.remote }

func mustAddr(t *testing.T, s string) ma.Multiaddr {
	t.Helper()
	m, err := ma.NewMultiaddr(s)
	if err != nil {
		t.Fatalf("%s: %v", s, err)
	}
	return m
}

func TestIsRelayed(t *testing.T) {
	cases := []struct {
		name string
		c    fakeConn
		want bool
	}{
		{"direct LAN", fakeConn{false, mustAddr(t, "/ip4/10.0.0.5/tcp/4001")}, false},
		{"direct TP", fakeConn{false, mustAddr(t, "/ip4/169.254.110.47/tcp/4001")}, false},
		{"direct WAN", fakeConn{false, mustAddr(t, "/ip4/203.0.113.10/tcp/16690")}, false},
		{"limited flag", fakeConn{true, mustAddr(t, "/ip4/10.0.0.5/tcp/4001")}, true},
		{"p2p-circuit addr", fakeConn{false, mustAddr(t, "/ip4/203.0.113.10/tcp/16690/p2p-circuit")}, true},
	}
	for _, c := range cases {
		if got := isRelayed(c.c); got != c.want {
			t.Errorf("%s: isRelayed = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestPeerLinkClassDerivation mirrors the strongest-class-across-direct-conns
// rule peerConns uses: a peer dialed over both Wi-Fi (lan) and a wired link
// (tp) reports the stronger "tp"; a relayed conn contributes no class.
func TestPeerLinkClassDerivation(t *testing.T) {
	conns := []fakeConn{
		{false, mustAddr(t, "/ip4/10.0.0.5/tcp/4001")},               // lan
		{false, mustAddr(t, "/ip4/169.254.110.47/udp/4001/quic-v1")}, // tp
		{true, mustAddr(t, "/ip4/203.0.113.10/tcp/16690")},           // relayed → ignored
	}
	best := ""
	direct := false
	for _, c := range conns {
		if isRelayed(c) {
			continue
		}
		direct = true
		best = strongerLinkClass(best, classifyConnAddr(c.RemoteMultiaddr()))
	}
	if !direct {
		t.Fatal("expected a direct connection")
	}
	if best != "tp" {
		t.Errorf("strongest link class = %q, want %q", best, "tp")
	}
}

func TestClassifyConnAddr(t *testing.T) {
	cases := []struct{ addr, want string }{
		{"/ip4/10.0.0.5/tcp/4001", "lan"},            // RFC-1918 wifi LAN
		{"/ip4/192.168.1.9/udp/4001/quic-v1", "lan"}, // RFC-1918 LAN
		{"/ip4/172.16.4.4/tcp/4001", "lan"},          // RFC-1918 LAN
		{"/ip4/169.254.110.47/tcp/4001", "tp"},       // APIPA / link-local = direct wired link
		{"/ip6/fe80::1/tcp/4001", "tp"},              // IPv6 link-local
		{"/ip4/203.0.113.10/tcp/16690", "wan"},       // public (RFC 5737 TEST-NET-3 doc range)
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

// TestLocalLANLabel covers the LOCAL-multiaddr → LAN-label mapping that names
// WHICH lan a direct link rides over (the class alone collapses every private
// network into "lan"): link-local ⇒ "wired", RFC-1918 ⇒ /24 subnet base,
// public/loopback ⇒ "".
func TestLocalLANLabel(t *testing.T) {
	cases := []struct{ addr, want string }{
		{"/ip4/169.254.13.7/tcp/4001", "wired"},      // APIPA link-local = wired crosslink
		{"/ip6/fe80::1/tcp/4001", "wired"},           // IPv6 link-local = wired crosslink
		{"/ip4/10.0.0.5/udp/4001/quic-v1", "10.0.0"}, // RFC-1918 /8 → subnet base
		{"/ip4/192.168.1.42/tcp/4001", "192.168.1"},  // RFC-1918 /16 → subnet base
		{"/ip4/172.16.4.9/tcp/4001", "172.16.4"},     // RFC-1918 /12 → subnet base
		{"/ip4/203.0.113.10/tcp/16690", ""},          // public (RFC 5737 TEST-NET-3) → no label
		{"/ip4/127.0.0.1/tcp/4001", ""},              // loopback → no label
	}
	for _, c := range cases {
		m, err := ma.NewMultiaddr(c.addr)
		if err != nil {
			t.Fatalf("%s: %v", c.addr, err)
		}
		if got := localLANLabel(m); got != c.want {
			t.Errorf("localLANLabel(%s) = %q, want %q", c.addr, got, c.want)
		}
	}
	if got := localLANLabel(nil); got != "" {
		t.Errorf("localLANLabel(nil) = %q, want \"\"", got)
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
