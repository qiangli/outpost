package plugin

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// IPAMDir is where per-container IP allocations are persisted as
// <container-id>.ip → "10.42.5.7" text files. State outlives the
// kubelet process — it's the authority on which IPs are free.
//
// 0o700 so non-root can't snoop pod IPs (defense-in-depth; pod IPs
// aren't secret but root-only is the right default).
const IPAMDir = "/var/lib/cloudbox/cni/ipam"

// AllocateIP grabs the next-free address in cidr and persists it. A
// process-shared file lock serializes the complete read/claim/commit
// transaction across concurrent CNI invocations.
// Skips .0 (network), .1 (bridge gateway), and the broadcast address.
// Idempotent on (containerID): if a file already exists, returns
// that IP — handy when kubelet retries ADD on a transient failure.
func AllocateIP(cidr, containerID string) (net.IP, error) {
	return allocateIP(IPAMDir, cidr, containerID)
}

func allocateIP(dir, cidr, containerID string) (net.IP, error) {
	if err := validateContainerID(containerID); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("ipam: mkdir %s: %w", dir, err)
	}
	unlock, err := lockIPAM(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()

	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("ipam: parse cidr: %w", err)
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

	path := filepath.Join(dir, containerID+".ip")
	if b, err := readAllocation(path); err == nil {
		ip := net.ParseIP(string(b)).To4()
		if validAllocation(ipnet, base, total, ip) {
			claimed, err := claimedByOther(dir, filepath.Base(path), ip.String())
			if err != nil {
				return nil, err
			}
			if !claimed {
				return ip, nil
			}
		}
	}

	used, err := loadUsed(dir)
	if err != nil {
		return nil, err
	}
	// .0 network, .1 bridge, .last broadcast — skip those.
	for offset := 2; offset < total-1; offset++ {
		candidate := nextIP(base, offset)
		if used[candidate.String()] {
			continue
		}
		if err := writeAllocation(path, []byte(candidate.String())); err != nil {
			return nil, fmt.Errorf("ipam: persist: %w", err)
		}
		return candidate, nil
	}
	return nil, fmt.Errorf("ipam: cidr %s exhausted", cidr)
}

// ReleaseIP removes the file for containerID. Best-effort; CNI DEL
// semantics expect idempotence so a missing file is fine.
func ReleaseIP(containerID string) error {
	return releaseIP(IPAMDir, containerID)
}

func releaseIP(dir, containerID string) error {
	if err := validateContainerID(containerID); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ipam: mkdir %s: %w", dir, err)
	}
	unlock, err := lockIPAM(dir)
	if err != nil {
		return err
	}
	defer unlock()

	path := filepath.Join(dir, containerID+".ip")
	err = os.Remove(path)
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
		if !strings.HasSuffix(e.Name(), ".ip") || !e.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		ip := net.ParseIP(string(b))
		if ip4 := ip.To4(); ip4 != nil {
			out[ip4.String()] = true
		}
	}
	return out, nil
}

func validAllocation(ipnet *net.IPNet, base net.IP, total int, ip net.IP) bool {
	if ip == nil || !ipnet.Contains(ip) {
		return false
	}
	return !ip.Equal(base) && !ip.Equal(nextIP(base, 1)) && !ip.Equal(nextIP(base, total-1))
}

func claimedByOther(dir, owner, address string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Name() == owner || !strings.HasSuffix(e.Name(), ".ip") || !e.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		ip := net.ParseIP(string(b)).To4()
		if ip != nil && ip.String() == address {
			return true, nil
		}
	}
	return false, nil
}

func validateContainerID(containerID string) error {
	if containerID == "" || containerID == "." || containerID == ".." ||
		strings.ContainsAny(containerID, `/\`) || filepath.Base(containerID) != containerID {
		return fmt.Errorf("ipam: invalid container ID %q", containerID)
	}
	return nil
}

func readAllocation(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("ipam: allocation %s is not a regular file", path)
	}
	return os.ReadFile(path)
}

func writeAllocation(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".allocation-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()

	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirFile.Close()
	return dirFile.Sync()
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
