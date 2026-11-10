package main

import (
	"context"
	"github.com/qiangli/outpost/internal/agent/postinstall"
	"os/exec"
)

// All host service-manager commands (including absolute Windows paths and
// runuser wrappers) go through this seam. Tests inject a fail-closed runner.
var serviceCommand = exec.Command

var serviceCommandsIsolated bool

// Power policy, linger, and reboot probes also invoke host service managers.
var hostPostinstallRunner = postinstall.Exec

func rejectTestPostinstallCommand(_ context.Context, c postinstall.Command) postinstall.Readback {
	serviceCommand(c[0], c[1:]...)
	panic("test postinstall command requires an injected runner")
}
