//go:build windows

package vknode

import (
	"context"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/qiangli/outpost/internal/bashypath"
)

// Podman on Windows normally forwards published ports to host loopback.
// Probe that exact port before considering a machine address: the API socket
// can belong to a machine whose IP is not reachable from the Windows host.
func podmanTransientHost(socket string) func(context.Context, int) string {
	machine := machineNameFromPodmanSocket(socket)
	var mu sync.Mutex
	var host string
	var checked time.Time
	resolveMachine := func(ctx context.Context) string {
		if machine == "" {
			return ""
		}
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
			out, err := exec.CommandContext(queryCtx, bin, "podman", "machine", "ssh", machine, "ip", "-4", "-o", "addr", "show", "scope", "global").Output()
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
		out, err := exec.CommandContext(queryCtx, "podman", "machine", "ssh", machine, "ip", "-4", "-o", "addr", "show", "scope", "global").Output()
		if err == nil {
			if ip := firstMachineIPv4(string(out)); ip != "" {
				host, checked = ip, time.Now()
				return host
			}
		}
		slog.Debug("vknode: resolve Podman machine address", "machine", machine, "err", err)
		return host
	}
	return func(ctx context.Context, port int) string {
		host, source := choosePublishedHost(ctx, port, resolveMachine, reachableTransientPort)
		slog.Info("vknode: selected transient app target", "host", host, "port", port, "source", source, "machine", machine)
		return host
	}
}

func reachableTransientPort(ctx context.Context, host string, port int) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
