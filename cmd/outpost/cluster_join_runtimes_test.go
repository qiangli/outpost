package main

import (
	"reflect"
	"testing"

	"github.com/spf13/cobra"
)

// parseJoinFlags runs a real argv through the real join flag set, so this
// tests what an operator types — including the Changed() checks that make
// "unspecified" distinguishable from "specified as empty".
func parseJoinFlags(t *testing.T, argv []string) (*cobra.Command, *clusterJoinFlags) {
	t.Helper()
	var f clusterJoinFlags
	cmd := &cobra.Command{Use: "join"}
	addClusterJoinFlags(cmd, &f)
	if err := cmd.Flags().Parse(argv); err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	return cmd, &f
}

func TestClusterJoinFlags_RuntimeSelection(t *testing.T) {
	tests := []struct {
		name        string
		argv        []string
		wantAgent   *bool
		wantVirtual []string
		wantErr     bool
	}{
		{
			name: "no runtime flags leaves both nil — the agent-only default",
			argv: []string{"10.0.0.5", "--token", "t"},
		},
		{
			name:        "--cluster-virtual selects vk backends",
			argv:        []string{"10.0.0.5", "--token", "t", "--cluster-virtual", "vk-podman,vk-native"},
			wantVirtual: []string{"vk-podman", "vk-native"},
		},
		{
			name:      "--cluster-agent=off deselects the agent runtime",
			argv:      []string{"10.0.0.5", "--token", "t", "--cluster-agent=off", "--cluster-virtual", "vk-podman"},
			wantAgent: ptrBool(false),

			wantVirtual: []string{"vk-podman"},
		},
		{
			name:      "--cluster-agent=on is explicit",
			argv:      []string{"10.0.0.5", "--token", "t", "--cluster-agent=on"},
			wantAgent: ptrBool(true),
		},
		{
			name:    "an unknown backend fails before any daemon round-trip",
			argv:    []string{"10.0.0.5", "--token", "t", "--cluster-virtual", "vk-nonesuch"},
			wantErr: true,
		},
		{
			name:    "a non-toggle --cluster-agent value is rejected",
			argv:    []string{"10.0.0.5", "--token", "t", "--cluster-agent=maybe"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envJoinToken, "")
			t.Setenv(envSTCPSecret, "")
			t.Setenv(envNodeToken, "")

			cmd, f := parseJoinFlags(t, tt.argv[1:])
			p, err := resolveJoinParams(cmd, tt.argv[0], f)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", p)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveJoinParams: %v", err)
			}
			if !reflect.DeepEqual(p.Agent, tt.wantAgent) {
				t.Errorf("Agent = %v, want %v", derefBool(p.Agent), derefBool(tt.wantAgent))
			}
			if !reflect.DeepEqual(p.Virtual, tt.wantVirtual) {
				t.Errorf("Virtual = %v, want %v", p.Virtual, tt.wantVirtual)
			}
		})
	}
}

// The runtime flags must also route the invocation to the peer-join path;
// otherwise `outpost cluster join --cluster-virtual vk-podman` against an
// already-joined worker would silently take the node-lifecycle rejoin instead.
func TestWantsPeerJoin_RuntimeFlagsCount(t *testing.T) {
	t.Setenv(envJoinToken, "")
	tests := []struct {
		name string
		f    clusterJoinFlags
		want bool
	}{
		{"bare rejoin", clusterJoinFlags{}, false},
		{"cluster-agent named", clusterJoinFlags{clusterAgent: "off"}, true},
		{"cluster-virtual named", clusterJoinFlags{clusterVirtual: []string{"vk-podman"}}, true},
		{"cluster-virtual emptied", clusterJoinFlags{clusterVirtual: []string{}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wantsPeerJoin("", &tt.f); got != tt.want {
				t.Errorf("wantsPeerJoin = %v, want %v", got, tt.want)
			}
		})
	}
}

// The status line names every selected Node, so an operator can tell a
// defaulted agent-only join from one they configured.
func TestRuntimeSummary(t *testing.T) {
	tests := []struct {
		agent   bool
		virtual []string
		want    string
	}{
		{true, nil, "agent"},
		{false, []string{"vk-podman"}, "vk-podman"},
		{true, []string{"vk-podman", "vk-native"}, "agent, vk-podman, vk-native"},
		{false, nil, "none selected"},
	}
	for _, tt := range tests {
		if got := runtimeSummary(tt.agent, tt.virtual); got != tt.want {
			t.Errorf("runtimeSummary(%v, %v) = %q, want %q", tt.agent, tt.virtual, got, tt.want)
		}
	}
}

func ptrBool(b bool) *bool { return &b }

func derefBool(p *bool) string {
	if p == nil {
		return "<nil>"
	}
	if *p {
		return "true"
	}
	return "false"
}
