//go:build !linux

package plugin

import (
	"errors"
	"net"
)

// Stubs so the package builds on non-Linux (developer Darwin builds,
// tooling that walks the whole tree). Actual CNI invocation only
// happens from kubelet on Linux nodes.

var errNotLinux = errors.New("outpost-cni: CNI plugin only runs on Linux")

func EnsureBridge(_ string, _ string) error { return errNotLinux }

func PlugPod(_ *Args, _ net.IP, _ *Config) error { return errNotLinux }

func UnplugPod(_ *Args) error { return nil }
