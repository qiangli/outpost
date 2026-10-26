//go:build windows

package vknode

import (
	"context"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/qiangli/outpost/internal/bashypath"
)

// Podman on Windows publishes ports inside its Linux machine. Some Podman
// versions fail to forward them to Windows loopback, while the machine IP
// remains reachable. Use the address of the machine owning our API socket.
func podmanTransientHost(socket string) func(context.Context) string {
	machine := machineNameFromPodmanSocket(socket)
	if machine == "" {
		return nil
	}
	var mu sync.Mutex
	var host string
	var checked time.Time
	return func(ctx context.Context) string {
		mu.Lock()
		defer mu.Unlock()
		if host != "" && time.Since(checked) < 30*time.Second {
			return host
		}
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		// Use the same managed bashy Podman that started the machine. A
		// standalone podman.exe may not be installed on Windows.
		if bin, _, err := bashypath.Find(nil); err == nil {
			out, err := exec.CommandContext(queryCtx, bin, "podman", "machine", "ssh", machine, "hostname", "-I").Output()
			if err == nil {
				if ip := firstMachineIPv4(string(out)); ip != "" {
					host, checked = ip, time.Now()
					return host
				}
			}
			slog.Debug("vknode: resolve Podman machine through bashy", "machine", machine, "err", err)
		}
		// Operator-owned Podman remains a fallback when this daemon did
		// not start the machine with the managed bashy binary.
		out, err := exec.CommandContext(queryCtx, "podman", "machine", "ssh", machine, "hostname", "-I").Output()
		if err == nil {
			if ip := firstMachineIPv4(string(out)); ip != "" {
				host, checked = ip, time.Now()
				return host
			}
		}
		slog.Debug("vknode: resolve Podman machine address", "machine", machine, "err", err)
		return host
	}
}
