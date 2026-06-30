package admincore

import "testing"

// The mesh direct-link class is the ground truth that overrides cloudbox's
// egress-IP location heuristic: tp/lan ⇒ same_lan (enriched to "same_lan via
// <lan>" when the LAN label is known, flat "same_lan" when it isn't), wan ⇒
// remote, "" (no direct link / relay-only) falls back to whatever cloudbox
// computed.
func TestOverrideLocation(t *testing.T) {
	cases := []struct {
		name        string
		cloudboxLoc string
		linkClass   string
		lan         string
		want        string
	}{
		{"link-local TP via wired", "remote", "tp", "wired", "same_lan via wired"},
		{"RFC1918 LAN via subnet", "remote", "lan", "10.0.0", "same_lan via 10.0.0"},
		{"LAN keeps same_lan via subnet", "same_lan", "lan", "192.168.1", "same_lan via 192.168.1"},
		{"TP no LAN label degrades to flat same_lan", "remote", "tp", "", "same_lan"},
		{"LAN no LAN label degrades to flat same_lan", "same_lan", "lan", "", "same_lan"},
		{"public WAN overrides same_lan", "same_lan", "wan", "", "remote"},
		{"public WAN keeps remote", "remote", "wan", "", "remote"},
		{"no direct link falls back to remote", "remote", "", "", "remote"},
		{"no direct link falls back to same_lan", "same_lan", "", "", "same_lan"},
		{"no direct link falls back to unknown", "unknown", "", "", "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := overrideLocation(c.cloudboxLoc, c.linkClass, c.lan); got != c.want {
				t.Errorf("overrideLocation(%q, %q, %q) = %q, want %q",
					c.cloudboxLoc, c.linkClass, c.lan, got, c.want)
			}
		})
	}
}
