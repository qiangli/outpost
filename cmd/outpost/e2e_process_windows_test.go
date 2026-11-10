package main

import (
	"os/exec"
)

// Real-daemon E2E tests are skipped on Windows.
func configureTestProcess(cmd *exec.Cmd) {}
func killTestProcessGroup(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
