package main

import (
	"os"
	"testing"
)

func TestSameExecutableName(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		want bool
	}{
		{self, true},
		{"outpost", true},
		{"outpost.exe", true},
		{"/opt/homebrew/opt/bashy/bin/outpost", true},
		{"ollama", false},
		{"bashy", false},
	} {
		if got := sameExecutableName(tc.name); got != tc.want {
			t.Errorf("sameExecutableName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A stale pidfile naming a live process that is not outpost (pid reuse after
// a reboot) must not block the supervisor; our own pid must.
func TestPidIsOutpostRejectsForeignLivePid(t *testing.T) {
	if pidIsOutpost(1) {
		t.Errorf("pid 1 (init/launchd) treated as a running outpost supervisor")
	}
}
