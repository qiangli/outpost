package main

import (
	"os"
	"testing"
)

func TestSupervisordProgramsOwnOnlyDaemonAndBashyMonitor(t *testing.T) {
	programs := supervisordPrograms("/opt/outpost", "/opt/bashy", "daemon.log", "bashy.log")
	if len(programs) != 2 {
		t.Fatalf("supervisord program count = %d, want exactly 2", len(programs))
	}
	if got := programs[0]; got.Name != "outpost" || got.Path != "/opt/outpost" || len(got.Args) != 1 || got.Args[0] != "start" {
		t.Fatalf("daemon program = %+v, want outpost start", got)
	}
	if got := programs[1]; got.Name != "bashy-supervisord" || got.Path != "/opt/bashy" || len(got.Args) != 1 || got.Args[0] != "supervisord" {
		t.Fatalf("bashy monitor program = %+v, want bashy supervisord", got)
	}
}

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
