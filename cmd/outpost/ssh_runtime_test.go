package main

import "testing"

// splitAdHocHostPort feeds the `outpost ssh user@host[:port]` direct-
// dial decision: a parsed port forces the plain-TCP LAN path, no port
// keeps the name on the cloudbox-assisted flow. IPv6 literals must
// not have their colons mistaken for a port separator.
func TestSplitAdHocHostPort(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
	}{
		{"host-c", "host-c", 0},
		{"192.168.1.5", "192.168.1.5", 0},
		{"192.168.1.5:2222", "192.168.1.5", 2222},
		{"host.local:22", "host.local", 22},
		{" host.local:2222 ", "host.local", 2222},
		{"[::1]:2222", "::1", 2222},
		{"::1", "::1", 0},         // bare IPv6 — colons are address bytes
		{"fe80::1", "fe80::1", 0}, // bare IPv6, multiple colons
		{"host:notaport", "host:notaport", 0},
		{"host:70000", "host:70000", 0}, // out of range → not a port
		{"host:", "host:", 0},           // empty port
		{":2222", ":2222", 0},           // no host — not an ad-hoc target
	}
	for _, tc := range cases {
		host, port := splitAdHocHostPort(tc.in)
		if host != tc.wantHost || port != tc.wantPort {
			t.Errorf("splitAdHocHostPort(%q) = (%q, %d), want (%q, %d)",
				tc.in, host, port, tc.wantHost, tc.wantPort)
		}
	}
}
