package vknode

import (
	"context"
	"net"
	"path/filepath"
	"strings"
)

// choosePublishedHost checks the actual published port before selecting a
// transient app target. A machine address from the API socket alone does not
// prove that a container port is reachable at that address.
func choosePublishedHost(ctx context.Context, port int, machineHost func(context.Context) string, reachable func(context.Context, string, int) bool) (host, source string) {
	const loopback = "127.0.0.1"
	if reachable(ctx, loopback, port) {
		return loopback, "host-loopback"
	}
	if machineHost != nil {
		if machine := machineHost(ctx); machine != "" && reachable(ctx, machine, port) {
			return machine, "podman-machine"
		}
	}
	// The workload may still be starting. Prefer the published host port
	// when neither address is listening yet, so a later attach can work.
	return loopback, "host-loopback-unverified"
}

// machineNameFromPodmanSocket identifies the machine that owns a Windows
// podman pipe or API socket. A generic docker_engine pipe is ambiguous.
func machineNameFromPodmanSocket(socket string) string {
	base := filepath.Base(strings.ReplaceAll(socket, "\\", "/"))
	var name string
	switch {
	case strings.HasSuffix(strings.ToLower(base), "-api.sock"):
		name = base[:len(base)-len("-api.sock")]
	case strings.HasPrefix(strings.ToLower(base), "podman-"):
		name = base[len("podman-"):]
	}
	if name == "" || !asciiLetterOrDigit(name[0]) {
		return ""
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !asciiLetterOrDigit(c) && c != '-' && c != '_' && c != '.' {
			return ""
		}
	}
	return name
}

func asciiLetterOrDigit(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func firstMachineIPv4(output string) string {
	for _, field := range strings.Fields(output) {
		ip := net.ParseIP(field)
		if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
			continue
		}
		return ip.String()
	}
	return ""
}
