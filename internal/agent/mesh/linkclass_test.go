package mesh

import (
	"reflect"
	"testing"

	"github.com/libp2p/go-libp2p/core/network"
	swarm "github.com/libp2p/go-libp2p/p2p/net/swarm"
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

// TestSameSubnet covers the IPv6-/64 and IPv4-/24 same-subnet test that lets a
// public address in the host's own subnet count as same-LAN. Doc ranges only
// (RFC 3849 2001:db8::/32, RFC 5737, RFC-1918).
func TestSameSubnet(t *testing.T) {
	cases := []struct {
		name          string
		local, remote string
		want          bool
	}{
		{"v6 same /64", "/ip6/2001:db8:1::1/tcp/4001", "/ip6/2001:db8:1::2/tcp/4001", true},
		{"v6 diff /64", "/ip6/2001:db8:1::1/tcp/4001", "/ip6/2001:db8:99::2/tcp/4001", false},
		{"v4 same /24", "/ip4/10.0.0.5/tcp/4001", "/ip4/10.0.0.9/tcp/4001", true},
		{"v4 diff /24", "/ip4/198.51.100.5/tcp/4001", "/ip4/203.0.113.10/tcp/4001", false},
		{"mixed family", "/ip4/10.0.0.5/tcp/4001", "/ip6/2001:db8:1::2/tcp/4001", false},
	}
	for _, c := range cases {
		got := sameSubnet(mustAddr(t, c.local), mustAddr(t, c.remote))
		if got != c.want {
			t.Errorf("%s: sameSubnet(%s,%s) = %v, want %v", c.name, c.local, c.remote, got, c.want)
		}
	}
}

// TestConnLink is the per-peer link logic PeerLinkInfo/PeerLinkClass run for one
// direct connection: REMOTE → class, LOCAL → label, with the same-subnet
// correction that turns a public-but-same-subnet "wan" into same-LAN. Doc
// ranges only.
func TestConnLink(t *testing.T) {
	cases := []struct {
		name               string
		local, remote      string
		wantClass, wantLAN string
	}{
		// Same IPv6 /64 over a public (RFC 3849 doc) GUA → same-LAN, "lan6"
		// label (never the raw /64 prefix).
		{"v6 same /64", "/ip6/2001:db8:1::1/tcp/4001", "/ip6/2001:db8:1::2/tcp/4001", "lan", "lan6"},
		// Different /64 → genuinely remote.
		{"v6 diff /64", "/ip6/2001:db8:1::1/tcp/4001", "/ip6/2001:db8:99::2/tcp/4001", "wan", ""},
		// RFC-1918 same /24 → existing behavior, classifyConnAddr already
		// says "lan"; label is the first-three-octet base.
		{"v4 rfc1918 /24", "/ip4/10.0.0.5/udp/4001/quic-v1", "/ip4/10.0.0.9/tcp/4001", "lan", "10.0.0"},
		// Public IPv4 in the host's own /24 (rare) → corrected to same-LAN.
		{"v4 public same /24", "/ip4/203.0.113.5/tcp/4001", "/ip4/203.0.113.9/tcp/4001", "lan", "203.0.113"},
		// Genuine public pair on different subnets → remote.
		{"v4 public diff subnet", "/ip4/198.51.100.5/tcp/4001", "/ip4/203.0.113.10/tcp/16690", "wan", ""},
		// Link-local stays "tp" (unchanged) regardless of subnet logic.
		{"tp link-local", "/ip4/169.254.1.1/tcp/4001", "/ip4/169.254.110.47/tcp/4001", "tp", "wired"},
	}
	for _, c := range cases {
		gotClass, gotLAN := connLink(mustAddr(t, c.local), mustAddr(t, c.remote))
		if gotClass != c.wantClass || gotLAN != c.wantLAN {
			t.Errorf("%s: connLink(%s,%s) = (%q,%q), want (%q,%q)",
				c.name, c.local, c.remote, gotClass, gotLAN, c.wantClass, c.wantLAN)
		}
	}
}

// TestIsLinkLocalV4 covers the wired-crosslink preference predicate: ONLY IPv4
// 169.254.0.0/16 (APIPA) is preferred. fe80 (IPv6 link-local) is deliberately
// NOT — it's undialable without a zone id. Doc/generic ranges only.
func TestIsLinkLocalV4(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"/ip4/169.254.10.5/udp/4001/quic-v1", true}, // APIPA wired crosslink
		{"/ip4/10.0.0.5/udp/4001/quic-v1", false},    // RFC-1918 Wi-Fi LAN
		{"/ip4/203.0.113.10/tcp/16690", false},       // public (RFC 5737 TEST-NET-3)
		{"/ip6/fe80::1/tcp/4001", false},             // IPv6 link-local — NOT preferred
		{"/ip6/2001:db8::1/tcp/4001", false},         // IPv6 doc GUA (RFC 3849)
	}
	for _, c := range cases {
		if got := isLinkLocalV4(mustAddr(t, c.addr)); got != c.want {
			t.Errorf("isLinkLocalV4(%s) = %v, want %v", c.addr, got, c.want)
		}
	}
}

// TestMeshDialRanker asserts the wired link (169.254) dials immediately while
// every other addr gets at least the head start — and that a peer with NO
// link-local addr is left exactly as swarm.DefaultDialRanker schedules it (the
// blast-radius guard). Doc/generic ranges only.
func TestMeshDialRanker(t *testing.T) {
	wired := mustAddr(t, "/ip4/169.254.10.5/udp/4001/quic-v1")
	wifi := mustAddr(t, "/ip4/10.0.0.5/udp/4001/quic-v1")
	pub := mustAddr(t, "/ip4/203.0.113.10/udp/4001/quic-v1")

	got := meshDialRanker([]ma.Multiaddr{wired, wifi, pub})
	for _, ad := range got {
		switch {
		case isLinkLocalV4(ad.Addr):
			if ad.Delay != 0 {
				t.Errorf("link-local %s: Delay = %v, want 0", ad.Addr, ad.Delay)
			}
		default:
			if ad.Delay < meshLinkLocalHeadStart {
				t.Errorf("non-link-local %s: Delay = %v, want >= %v", ad.Addr, ad.Delay, meshLinkLocalHeadStart)
			}
		}
	}

	// No link-local present → identical to DefaultDialRanker (unperturbed).
	noLL := []ma.Multiaddr{wifi, pub}
	if !reflect.DeepEqual(meshDialRanker(noLL), swarm.DefaultDialRanker(noLL)) {
		t.Errorf("meshDialRanker perturbed a pure-Wi-Fi/remote peer; want DefaultDialRanker unchanged")
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
