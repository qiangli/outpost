package admincore

import "testing"

// The mesh direct-link class is the ground truth that overrides cloudbox's
// egress-IP location heuristic: tp/lan ⇒ same_lan, wan ⇒ remote, "" (no direct
// link / relay-only) falls back to whatever cloudbox computed.
func TestOverrideLocation(t *testing.T) {
	cases := []struct {
		name        string
		cloudboxLoc string
		linkClass   string
		want        string
	}{
		{"link-local TP overrides remote", "remote", "tp", "same_lan"},
		{"RFC1918 LAN overrides remote", "remote", "lan", "same_lan"},
		{"LAN keeps same_lan", "same_lan", "lan", "same_lan"},
		{"public WAN overrides same_lan", "same_lan", "wan", "remote"},
		{"public WAN keeps remote", "remote", "wan", "remote"},
		{"no direct link falls back to remote", "remote", "", "remote"},
		{"no direct link falls back to same_lan", "same_lan", "", "same_lan"},
		{"no direct link falls back to unknown", "unknown", "", "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := overrideLocation(c.cloudboxLoc, c.linkClass); got != c.want {
				t.Errorf("overrideLocation(%q, %q) = %q, want %q",
					c.cloudboxLoc, c.linkClass, got, c.want)
			}
		})
	}
}
