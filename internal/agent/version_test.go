package agent

import "testing"

func TestBuildInfoShort(t *testing.T) {
	cases := []struct {
		in   BuildInfo
		want string
	}{
		{BuildInfo{}, "unknown"},
		{BuildInfo{Commit: "06d8d8912345abc"}, "06d8d89"},
		{BuildInfo{Commit: "06d8d8912345abc", Dirty: true}, "06d8d89-dirty"},
		{BuildInfo{Commit: "abc"}, "abc"},
	}
	for _, tc := range cases {
		if got := tc.in.Short(); got != tc.want {
			t.Errorf("Short(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestReadBuildInfoGoVersion(t *testing.T) {
	// Under `go test` the runtime always reports a Go version. Commit/VCS
	// fields may legitimately be empty (tests run without VCS settings
	// stamped), so we only assert the always-populated field.
	if got := ReadBuildInfo(); got.GoVersion == "" {
		t.Error("ReadBuildInfo().GoVersion is empty")
	}
}
