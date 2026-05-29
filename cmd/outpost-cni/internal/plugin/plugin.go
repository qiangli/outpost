// Package plugin contains the load-bearing logic for the outpost-cni
// binary, factored out so the tiny main package stays under 100 lines.
//
// Linux-only. The CNI binary will never run on non-Linux hosts (kubelet
// only invokes CNI plugins on Linux nodes); a non-Linux stub keeps the
// package buildable for tooling that walks the whole tree.
package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

// Config is the JSON kubelet hands us on stdin. The cniVersion +
// type fields are CNI-spec required; pod_cidr + bridge_name are our
// custom fields the conflist generator wires in.
type Config struct {
	CNIVersion string `json:"cniVersion"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	PodCIDR    string `json:"pod_cidr"`
	BridgeName string `json:"bridge_name,omitempty"`
}

// Args is the subset of CNI env vars we use.
type Args struct {
	ContainerID string
	Netns       string
	IfName      string
}

// LoadInputs parses stdin (CNI config JSON) + the standard CNI env
// vars. Returns (cfg, args, err).
func LoadInputs() (*Config, *Args, error) {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, nil, fmt.Errorf("read stdin: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, nil, fmt.Errorf("parse CNI config: %w", err)
	}
	if cfg.BridgeName == "" {
		cfg.BridgeName = "cbox0"
	}
	if cfg.PodCIDR == "" {
		return &cfg, nil, errors.New("CNI config missing pod_cidr")
	}
	args := &Args{
		ContainerID: os.Getenv("CNI_CONTAINERID"),
		Netns:       os.Getenv("CNI_NETNS"),
		IfName:      os.Getenv("CNI_IFNAME"),
	}
	if args.IfName == "" {
		args.IfName = "eth0"
	}
	return &cfg, args, nil
}

// MaskBits extracts the prefix-length string from a CIDR. "10.42.5.0/24" → "24".
func MaskBits(cidr string) string {
	parts := strings.SplitN(cidr, "/", 2)
	if len(parts) != 2 {
		return "24"
	}
	return parts[1]
}

// BridgeIP returns the .1 address of the pod CIDR — the bridge's
// gateway address that pods use as their default route.
func BridgeIP(cidr string) net.IP {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return net.IPv4(127, 0, 0, 1)
	}
	ip := make(net.IP, len(ipnet.IP))
	copy(ip, ipnet.IP)
	ip[len(ip)-1] = 1
	return ip
}
