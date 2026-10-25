package main

import (
	"os"
	"path/filepath"
	"testing"
)

// isolateMarkers points HOME/XDG at temp dirs so marker files never touch
// the real host (and never install anything).
func isolateMarkers(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

func TestDaemonShouldRun_CleanHost(t *testing.T) {
	isolateMarkers(t)
	if !daemonShouldRun() {
		t.Fatal("daemonShouldRun = false on a clean host; nothing wrote an intent")
	}
}

func TestDaemonShouldRun_StopIntentParks(t *testing.T) {
	isolateMarkers(t)
	if err := writeStopIntent(); err != nil {
		t.Fatalf("writeStopIntent: %v", err)
	}
	if daemonShouldRun() {
		t.Fatal("daemonShouldRun = true with a stop intent on disk; `outpost stop` would be restarted")
	}
}

func TestDaemonShouldRun_OnlyStopWritesIntent(t *testing.T) {
	isolateMarkers(t)
	// A kill -9 / crash writes nothing: no marker file appears on its
	// own, so the supervisor restarts. Proved here by the absence of
	// any marker after doing nothing at all.
	if p, err := stopIntentPath(); err != nil {
		t.Fatalf("stopIntentPath: %v", err)
	} else if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("stop intent exists without `outpost stop`: %v", err)
	}
	if !daemonShouldRun() {
		t.Fatal("daemonShouldRun = false with no intent; kills/crashes must restart")
	}
}

func TestStopIntent_ClearsAtBootUnlessDisabled(t *testing.T) {
	isolateMarkers(t)
	if err := writeStopIntent(); err != nil {
		t.Fatalf("writeStopIntent: %v", err)
	}
	if err := writeDisableMarker(); err != nil {
		t.Fatalf("writeDisableMarker: %v", err)
	}
	clearStopIntent() // what runSupervisord does at every boot
	if p, _ := stopIntentPath(); p != "" {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("stop intent survived the boot clear")
		}
	}
	if daemonShouldRun() {
		t.Fatal("daemonShouldRun = true with a disable marker; disable must survive boots")
	}
	clearDisableMarker() // what `outpost enable` does
	if !daemonShouldRun() {
		t.Fatal("daemonShouldRun = false after enable cleared both markers")
	}
}

func TestStopIntent_LivesNextToKnownDirs(t *testing.T) {
	isolateMarkers(t)
	cache, _ := stopIntentPath()
	if dir := filepath.Dir(cache); dir == "" || dir == "." {
		t.Fatalf("stop intent has no cache dir: %q", cache)
	}
	if err := writeStopIntent(); err != nil {
		t.Fatalf("writeStopIntent: %v", err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("stop intent not on disk at %s: %v", cache, err)
	}
}

func TestSupervisordProgramsWiresShouldRun(t *testing.T) {
	isolateMarkers(t)
	programs := supervisordPrograms("/opt/outpost", "/opt/bashy", "", "")
	if len(programs) != 2 {
		t.Fatalf("supervisordPrograms returned %d programs, want 2", len(programs))
	}
	if programs[0].ShouldRun == nil {
		t.Fatal("daemon program has no ShouldRun gate — `outpost stop` would always be restarted")
	}
	if !programs[0].ShouldRun() {
		t.Fatal("ShouldRun = false on a clean host")
	}
	if err := writeStopIntent(); err != nil {
		t.Fatal(err)
	}
	if programs[0].ShouldRun() {
		t.Fatal("ShouldRun = true with a stop intent on disk")
	}
	if programs[1].ShouldRun != nil {
		t.Fatal("bashy monitor must stay ungated (stop parks the daemon, not the monitor)")
	}
}
