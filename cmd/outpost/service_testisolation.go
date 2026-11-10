//go:build outpost_testisolated

package main

import (
	"fmt"
	"os"
	"os/exec"
)

// Test-built binaries cannot reach any host service manager, even if a caller
// forgets HOME/PATH isolation. This tag must never be used for release builds.
func init() {
	serviceCommandsIsolated = true
	hostPostinstallRunner = rejectTestPostinstallCommand
	label := os.Getenv("OUTPOST_TEST_SERVICE_LABEL")
	if label == "" {
		label = fmt.Sprintf("outpost-test-%d", os.Getpid())
	}
	launchdLabel = "io.dhnt.test." + label
	systemdUnit = "test-" + label + ".service"
	windowsTask = "test-" + label
	serviceCommand = func(name string, args ...string) *exec.Cmd {
		msg := fmt.Sprintf("blocked host service command in test binary: %s %q", name, args)
		if path := os.Getenv("OUTPOST_TEST_SERVICE_AUDIT"); path != "" {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if err == nil {
				_, _ = fmt.Fprintln(f, msg)
				_ = f.Close()
			}
		}
		panic(msg) // platform callers sometimes ignore Run errors; never return one
	}
}
