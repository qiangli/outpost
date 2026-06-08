package main

import "testing"

func TestParseSCPArg(t *testing.T) {
	cases := []struct {
		in   string
		want scpEndpoint
	}{
		// Plain local paths (no colon, or colon after a slash).
		{"foo", scpEndpoint{Path: "foo"}},
		{"./foo", scpEndpoint{Path: "./foo"}},
		{"/abs/path", scpEndpoint{Path: "/abs/path"}},
		{"", scpEndpoint{Path: ""}},

		// scp-style: leading colon-without-slash means remote.
		{"host:foo", scpEndpoint{Remote: true, Host: "host", Path: "foo"}},
		{"host:/abs/path", scpEndpoint{Remote: true, Host: "host", Path: "/abs/path"}},
		{"host:", scpEndpoint{Remote: true, Host: "host", Path: ""}},

		// user@host: form.
		{"alice@dragon:foo", scpEndpoint{Remote: true, User: "alice", Host: "dragon", Path: "foo"}},
		{"alice@dragon:/abs/path", scpEndpoint{Remote: true, User: "alice", Host: "dragon", Path: "/abs/path"}},

		// scp's local-with-colon escape: a slash before the colon
		// forces local interpretation (so files named "foo:bar"
		// can be referenced as "./foo:bar").
		{"./foo:bar", scpEndpoint{Path: "./foo:bar"}},
		{"/abs/foo:bar", scpEndpoint{Path: "/abs/foo:bar"}},
		{"sub/dir/foo:bar", scpEndpoint{Path: "sub/dir/foo:bar"}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := parseSCPArg(tc.in)
			if got != tc.want {
				t.Fatalf("parseSCPArg(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}
