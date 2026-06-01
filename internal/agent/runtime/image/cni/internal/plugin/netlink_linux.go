//go:build linux

package plugin

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
)

// EnsureBridge creates the per-node Linux bridge if missing and
// assigns it the .1 address from the pod CIDR. Idempotent — safe
// to call on every CNI ADD. Uses iproute2 (`ip` command) rather
// than vishvananda/netlink to keep the CNI binary's footprint
// minimal; iproute2 is on every reasonable Linux distro k3s runs on.
//
// Also enables IP forwarding (sysctl net.ipv4.ip_forward=1) since
// the bridge wouldn't forward pod packets otherwise. Idempotent.
func EnsureBridge(name, podCIDR string) error {
	gw := BridgeIP(podCIDR)
	mask := MaskBits(podCIDR)
	gwCIDR := gw.String() + "/" + mask

	// Bridge create — `ip link add` is idempotent-with-stderr; we
	// swallow "file exists" so the second CNI ADD doesn't fail.
	if out, err := exec.Command("ip", "link", "add", name, "type", "bridge").CombinedOutput(); err != nil {
		if !strings.Contains(string(out), "exists") {
			return fmt.Errorf("ip link add %s: %w (%s)", name, err, strings.TrimSpace(string(out)))
		}
	}
	if out, err := exec.Command("ip", "link", "set", name, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("ip link set up: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	// addr add is also non-idempotent; ignore "exists".
	if out, err := exec.Command("ip", "addr", "add", gwCIDR, "dev", name).CombinedOutput(); err != nil {
		if !strings.Contains(string(out), "exists") {
			return fmt.Errorf("ip addr add %s: %w (%s)", gwCIDR, err, strings.TrimSpace(string(out)))
		}
	}
	// IP forwarding — sysctl is idempotent; this just keeps it set
	// in case some other component flipped it off.
	if out, err := exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").CombinedOutput(); err != nil {
		return fmt.Errorf("enable ip_forward: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// PlugPod creates the veth pair, moves one end into the pod's netns
// with the assigned IP + gateway, and attaches the host end to the
// bridge.
func PlugPod(args *Args, ip net.IP, cfg *Config) error {
	hostVeth := vethName(args.ContainerID, "h")
	podVeth := vethName(args.ContainerID, "p")

	// Create veth pair (host end + pod end).
	if out, err := exec.Command("ip", "link", "add", hostVeth,
		"type", "veth", "peer", "name", podVeth).CombinedOutput(); err != nil {
		return fmt.Errorf("ip link add veth: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	// Move pod end into the pod's netns.
	if out, err := exec.Command("ip", "link", "set", podVeth,
		"netns", args.Netns).CombinedOutput(); err != nil {
		return fmt.Errorf("ip link set netns: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	// Attach host end to bridge + bring up.
	if out, err := exec.Command("ip", "link", "set", hostVeth,
		"master", cfg.BridgeName).CombinedOutput(); err != nil {
		return fmt.Errorf("ip link set master: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("ip", "link", "set", hostVeth, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("ip link set host up: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	// Inside the pod netns: rename pod-end to IfName (eth0), assign IP,
	// add default route via bridge gateway. nsenter handles the netns
	// jump in one shot.
	gwAddr := BridgeIP(cfg.PodCIDR).String()
	ipCIDR := ip.String() + "/" + MaskBits(cfg.PodCIDR)
	netnsPath := args.Netns
	commands := []string{
		fmt.Sprintf("ip link set %s name %s", podVeth, args.IfName),
		fmt.Sprintf("ip link set %s up", args.IfName),
		fmt.Sprintf("ip addr add %s dev %s", ipCIDR, args.IfName),
		fmt.Sprintf("ip route add default via %s dev %s", gwAddr, args.IfName),
	}
	for _, c := range commands {
		parts := append([]string{"--net=" + netnsPath, "--"}, strings.Split(c, " ")...)
		if out, err := exec.Command("nsenter", parts...).CombinedOutput(); err != nil {
			return fmt.Errorf("nsenter %s: %w (%s)", c, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// UnplugPod removes the host-end veth. The pod-end disappears with
// the pod's netns. Best-effort: failures are swallowed (CNI DEL must
// be idempotent and tolerant of "already cleaned up" states).
func UnplugPod(args *Args) error {
	hostVeth := vethName(args.ContainerID, "h")
	_ = exec.Command("ip", "link", "del", hostVeth).Run()
	return nil
}

// vethName generates a unique-per-container veth name. Linux iface
// names cap at 15 chars; we use a short prefix + first 11 hex chars
// of the container ID + h/p side suffix.
func vethName(containerID, side string) string {
	id := containerID
	if len(id) > 11 {
		id = id[:11]
	}
	return "vbx" + side + id
}
