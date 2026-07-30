package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const retainedIPAMQuarantines = 3

// QuarantineStaleIPAM moves the complete lease directory aside and creates a
// fresh one. Call only when the outer supervisor has proved that the inner
// containerd was recreated: every old sandbox disappeared with it, so none of
// its leases can still be live.
func QuarantineStaleIPAM() (string, error) {
	return quarantineStaleIPAM(IPAMDir, time.Now().UTC(), retainedIPAMQuarantines)
}

func quarantineStaleIPAM(dir string, now time.Time, keep int) (string, error) {
	if keep < 1 {
		return "", fmt.Errorf("ipam: quarantine retention must be positive")
	}
	root := filepath.Dir(dir)
	if err := ensureRealDir(root); err != nil {
		return "", err
	}
	quarantineRoot := filepath.Join(root, "ipam-quarantine")
	if err := ensureRealDir(quarantineRoot); err != nil {
		return "", err
	}

	var moved string
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("ipam: lease path %s is not a real directory", dir)
		}
		name := "ipam-" + now.Format("20060102T150405.000000000Z")
		moved = filepath.Join(quarantineRoot, name)
		if err := os.Rename(dir, moved); err != nil {
			return "", fmt.Errorf("ipam: quarantine stale leases: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("ipam: inspect lease directory: %w", err)
	}

	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return moved, fmt.Errorf("ipam: create fresh lease directory: %w", err)
	}
	if err := syncDir(root); err != nil {
		return moved, err
	}
	if err := pruneIPAMQuarantines(quarantineRoot, keep); err != nil {
		return moved, err
	}
	return moved, nil
}

func ensureRealDir(dir string) error {
	info, err := os.Lstat(dir)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("ipam: create %s: %w", dir, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("ipam: inspect %s: %w", dir, err)
	case !info.IsDir() || info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("ipam: %s is not a real directory", dir)
	default:
		return nil
	}
}

func pruneIPAMQuarantines(root string, keep int) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 ||
			!strings.HasPrefix(entry.Name(), "ipam-") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if len(names) <= keep {
		return syncDir(root)
	}
	for _, name := range names[keep:] {
		target := filepath.Join(root, name)
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("ipam: prune quarantine %s: %w", target, err)
		}
	}
	return syncDir(root)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
