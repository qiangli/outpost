package main

import (
	"io"
	"strings"
	"testing"
)

// TestAppsAddExposeFlagsValidated: `outpost apps add` rejects a
// malformed --expose-token-sha256 and refuses either expose flag on its
// own, before it ever dials the daemon. The error never echoes the
// supplied hash.
func TestAppsAddExposeFlagsValidated(t *testing.T) {
	const badHash = "abc123"
	const goodHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "bad hex",
			args: []string{"add", "web", "--url", "http://127.0.0.1:9000",
				"--expose-token-sha256", badHash, "--expose-expires", "2026-01-01T00:00:00Z"},
			want: "64 hex",
		},
		{
			name: "odd length hex",
			args: []string{"add", "web", "--url", "http://127.0.0.1:9000",
				"--expose-token-sha256", "zz" + goodHash[2:], "--expose-expires", "2026-01-01T00:00:00Z"},
			want: "64 hex",
		},
		{
			name: "hash without expires",
			args: []string{"add", "web", "--url", "http://127.0.0.1:9000",
				"--expose-token-sha256", goodHash},
			want: "together",
		},
		{
			name: "expires without hash",
			args: []string{"add", "web", "--url", "http://127.0.0.1:9000",
				"--expose-expires", "2026-01-01T00:00:00Z"},
			want: "together",
		},
		{
			name: "bad expires",
			args: []string{"add", "web", "--url", "http://127.0.0.1:9000",
				"--expose-token-sha256", goodHash, "--expose-expires", "tomorrow"},
			want: "RFC3339",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := appsCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("args %v accepted, want a validation error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), badHash) || strings.Contains(err.Error(), goodHash) {
				t.Errorf("error echoes the supplied hash: %q", err)
			}
		})
	}
}
