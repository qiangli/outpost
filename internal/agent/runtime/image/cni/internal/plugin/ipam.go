package plugin

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// IPAMDir is where per-container IP allocations are persisted as
// <container-id>.ip → "10.42.5.7" text files. State outlives the
// kubelet process — it's the authority on which IPs are free.
//
// 0o700 so non-root can't snoop pod IPs (defense-in-depth; pod IPs
// aren't secret but root-only is the right default).
const IPAMDir = "/var/lib/cloudbox/cni/ipam"

// AllocateIP grabs the next-free address in cidr and persists it.
// Skips .0 (network), .1 (bridge gateway), and the broadcast address.
// Idempotent on (containerID): if a file already exists, returns
// that IP — handy when kubelet retries ADD on a transient failure.
func AllocateIP(cidr, containerID string) (net.IP, error) {
	if err := os.MkdirAll(IPAMDir, 0o700); err != nil {
		return nil, fmt.Errorf("ipam: mkdir %s: %w", IPAMDir, err)
	}
	path := filepath.Join(IPAMDir, containerID+".ip")
	if b, err := os.ReadFile(path); err == nil {
		ip := net.ParseIP(string(b))
		if ip != nil {
			return ip.To4(), nil
		}
	}

	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("ipam: parse cidr: %w", err)
	}
	used, err := loadUsed(IPAMDir)
	if err != nil {
		return nil, err
	}
	base := ipnet.IP.To4()
	if base == nil {
		return nil, errors.New("ipam: only IPv4 supported in v1")
	}
	ones, bits := ipnet.Mask.Size()
	hostBits := bits - ones
	if hostBits < 2 {
		return nil, errors.New("ipam: cidr too small (need at least /30)")
	}
	total := 1 << uint(hostBits)
	// .0 network, .1 bridge, .last broadcast — skip those.
	for offset := 2; offset < total-1; offset++ {
		candidate := nextIP(base, offset)
		if used[candidate.String()] {
			continue
		}
		if err := os.WriteFile(path, []byte(candidate.String()), 0o600); err != nil {
			return nil, fmt.Errorf("ipam: persist: %w", err)
		}
		return candidate, nil
	}
	return nil, fmt.Errorf("ipam: cidr %s exhausted", cidr)
}

// ReleaseIP removes the file for containerID. Best-effort; CNI DEL
// semantics expect idempotence so a missing file is fine.
func ReleaseIP(containerID string) error {
	path := filepath.Join(IPAMDir, containerID+".ip")
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func loadUsed(dir string) (map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	out := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		ip := net.ParseIP(string(b))
		if ip != nil {
			out[ip.To4().String()] = true
		}
	}
	return out, nil
}

// nextIP returns base + offset as a 4-byte IPv4.
func nextIP(base net.IP, offset int) net.IP {
	out := make(net.IP, 4)
	copy(out, base.To4())
	// Treat the address as a big-endian uint32; add offset.
	v := uint32(out[0])<<24 | uint32(out[1])<<16 | uint32(out[2])<<8 | uint32(out[3])
	v += uint32(offset)
	out[0] = byte(v >> 24)
	out[1] = byte(v >> 16)
	out[2] = byte(v >> 8)
	out[3] = byte(v)
	return out
}
