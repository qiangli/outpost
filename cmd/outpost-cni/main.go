// Command outpost-cni implements a minimal Container Network Interface
// (CNI) plugin for Phase 3 of the outpost overlay design. Each pod
// gets a veth pair: one end in the pod's netns with the pod IP, the
// other end attached to a per-node Linux bridge ("cbox0"). The bridge
// is connected to the tailscale0 device via routing — pod CIDRs from
// other outposts are reachable because their tailscaled advertises
// them as `--advertise-routes`, which our local tailscaled installs
// as kernel routes via tailscale0 once it accepts them.
//
// IPAM: per-pod IPs are allocated from the node's PodCIDR by walking
// the address range and grabbing the first free address. State lives
// at /var/lib/cloudbox/cni/ipam/<container-id>.ip — kubelet drives
// ADD/DEL so the file is created in ADD and removed in DEL.
//
// Spec: CNI 0.4.0 minimal (ADD, DEL, CHECK, VERSION). Kubelet invokes
// this binary with a JSON config blob on stdin and CNI_COMMAND in env.
// The output is a JSON CNI Result on stdout.
//
// This is intentionally tiny (~300 LOC) — we don't aim to be a full
// CNI like Calico or Cilium, just enough plumbing to let pods get IPs
// and route to other pods through the tailnet. NetworkPolicy enforcement
// is out of scope for Phase 3 MVP.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"

	"github.com/qiangli/outpost/cmd/outpost-cni/internal/plugin"
)

const supportedCNIVersion = "0.4.0"

func main() {
	cmd := os.Getenv("CNI_COMMAND")
	switch cmd {
	case "ADD":
		if err := doAdd(); err != nil {
			emitError(1, err.Error())
		}
	case "DEL":
		if err := doDel(); err != nil {
			emitError(2, err.Error())
		}
	case "CHECK":
		if err := doCheck(); err != nil {
			emitError(3, err.Error())
		}
	case "VERSION":
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"cniVersion":        supportedCNIVersion,
			"supportedVersions": []string{"0.3.0", "0.3.1", "0.4.0"},
		})
	default:
		emitError(4, "unsupported CNI_COMMAND: "+cmd)
	}
}

func doAdd() error {
	cfg, args, err := plugin.LoadInputs()
	if err != nil {
		return err
	}
	ip, err := plugin.AllocateIP(cfg.PodCIDR, args.ContainerID)
	if err != nil {
		return fmt.Errorf("ipam allocate: %w", err)
	}
	if err := plugin.EnsureBridge(cfg.BridgeName, cfg.PodCIDR); err != nil {
		_ = plugin.ReleaseIP(args.ContainerID)
		return fmt.Errorf("bridge setup: %w", err)
	}
	if err := plugin.PlugPod(args, ip, cfg); err != nil {
		_ = plugin.ReleaseIP(args.ContainerID)
		return fmt.Errorf("plug pod: %w", err)
	}
	out := map[string]any{
		"cniVersion": supportedCNIVersion,
		"interfaces": []map[string]any{{
			"name":    args.IfName,
			"sandbox": args.Netns,
		}},
		"ips": []map[string]any{{
			"version":   "4",
			"address":   ip.String() + "/" + plugin.MaskBits(cfg.PodCIDR),
			"gateway":   plugin.BridgeIP(cfg.PodCIDR).String(),
			"interface": 0,
		}},
		"routes": []map[string]any{{"dst": "0.0.0.0/0"}},
		"dns":    map[string]any{},
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

func doDel() error {
	_, args, err := plugin.LoadInputs()
	if err != nil {
		// CNI spec says DEL should be best-effort; surface but don't
		// fatal-out on missing inputs.
		return err
	}
	_ = plugin.UnplugPod(args)
	_ = plugin.ReleaseIP(args.ContainerID)
	return nil
}

func doCheck() error {
	// Minimal CHECK: load + return ok if config parses. Full
	// CHECK would re-verify the pod's interface exists with the
	// expected IP; not load-bearing for our use case.
	_, _, err := plugin.LoadInputs()
	return err
}

func emitError(code int, msg string) {
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"cniVersion": supportedCNIVersion,
		"code":       code,
		"msg":        msg,
	})
	os.Exit(1)
}

// ensure the net import isn't dropped if plugin doesn't pull it in.
var _ = net.IPv4zero
