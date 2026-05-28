package main

import (
	"reflect"
	"testing"
)

func TestRewriteWildcardHost(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Bare host:port — the inherited-from-launchd form that
		// surfaced the original "Forbidden: invalid Host header
		// "0.0.0.0:17777"" report.
		{"0.0.0.0:17777", "127.0.0.1:17777"},
		// Scheme-prefixed: same rewrite, scheme preserved.
		{"http://0.0.0.0:17777", "http://127.0.0.1:17777"},
		{"https://0.0.0.0:17777", "https://127.0.0.1:17777"},
		// IPv6 wildcard — same Go-stdlib quirk.
		{"[::]:17777", "[::1]:17777"},
		{"http://[::]:17777", "http://[::1]:17777"},
		// Loopback / DNS / non-wildcard addrs pass through unchanged.
		{"127.0.0.1:17777", "127.0.0.1:17777"},
		{"http://127.0.0.1:17777", "http://127.0.0.1:17777"},
		{"outpost.example.com:17777", "outpost.example.com:17777"},
		{"https://outpost.example.com", "https://outpost.example.com"},
		// Empty string survives — caller's job to validate.
		{"", ""},
	}
	for _, c := range cases {
		got := rewriteWildcardHost(c.in)
		if got != c.want {
			t.Errorf("rewriteWildcardHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeMCPEndpoint_WildcardRewrite(t *testing.T) {
	// End-to-end: the dial URL the SDK gets fed should already be
	// loopback-rooted when the operator's $OUTPOST_ADMIN_ADDR is the
	// LAN-bind sentinel.
	got := normalizeMCPEndpoint("0.0.0.0:17777")
	want := "http://127.0.0.1:17777/mcp"
	if got != want {
		t.Errorf("normalizeMCPEndpoint(\"0.0.0.0:17777\") = %q, want %q", got, want)
	}
}

func TestSplitOutpostFlags(t *testing.T) {
	cases := []struct {
		name        string
		in          []string
		wantArgs    []string
		wantRefresh bool
	}{
		{
			name:        "no outpost flags",
			in:          []string{"get", "pods", "-n", "default"},
			wantArgs:    []string{"get", "pods", "-n", "default"},
			wantRefresh: false,
		},
		{
			name:        "refresh consumed",
			in:          []string{"--refresh", "get", "pods"},
			wantArgs:    []string{"get", "pods"},
			wantRefresh: true,
		},
		{
			name:        "refresh anywhere",
			in:          []string{"get", "--refresh", "pods"},
			wantArgs:    []string{"get", "pods"},
			wantRefresh: true,
		},
		{
			name: "after -- separator, refresh is for kubectl",
			// The literal `--` makes everything after it a kubectl
			// arg even if it spells the same name as our flag.
			// Lets `kubectl exec POD -- --refresh whatever` work.
			in:          []string{"exec", "pod", "--", "--refresh"},
			wantArgs:    []string{"exec", "pod", "--", "--refresh"},
			wantRefresh: false,
		},
		{
			name:        "refresh before -- still consumed",
			in:          []string{"--refresh", "exec", "pod", "--", "uname"},
			wantArgs:    []string{"exec", "pod", "--", "uname"},
			wantRefresh: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotArgs, gotRefresh := splitOutpostFlags(c.in)
			if !reflect.DeepEqual(gotArgs, c.wantArgs) {
				t.Errorf("args = %v, want %v", gotArgs, c.wantArgs)
			}
			if gotRefresh != c.wantRefresh {
				t.Errorf("refresh = %v, want %v", gotRefresh, c.wantRefresh)
			}
		})
	}
}
