package heartbeat

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func collectDisks(ctx context.Context) []measurement {
	guests := make(chan []measurement, 1)
	go func() { guests <- wslVolumes(ctx) }()
	var buf [windows.MAX_PATH]uint16
	h, err := windows.FindFirstVolume(&buf[0], uint32(len(buf)))
	volumes := map[string]measurement{}
	if err == nil {
		defer windows.FindVolumeClose(h)
		for len(volumes) < maxVolumes {
			var total, free uint64
			if windows.GetDriveType(&buf[0]) == windows.DRIVE_FIXED && windows.GetDiskFreeSpaceEx(&buf[0], nil, &total, &free) == nil && total > 0 && free <= total {
				volume := windows.UTF16ToString(buf[:])
				id := volumeID("win", strings.ToLower(volume))
				drive := driveLetter(&buf[0])
				display := drive
				if display == "" {
					display = strings.TrimSuffix(strings.TrimPrefix(volume, `\\?\`), `\`)
				}
				volumes[id] = measurement{diskVolume: diskVolume{ID: id, DisplayName: display, Drive: drive, Total: total, Used: total - free}, sizes: map[string]uint64{}}
			}
			if windows.FindNextVolume(h, &buf[0], uint32(len(buf))) != nil {
				break
			}
		}
	}

	failed := map[[2]string]bool{}
	for label, paths := range windowsStores() {
		for _, p := range paths {
			id := windowsVolumeID(p)
			m, ok := volumes[id]
			if !ok {
				continue
			}
			walkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			size, err := treeSize(walkCtx, p)
			cancel()
			if err == nil {
				m.sizes[label] += size
			} else if !os.IsNotExist(err) {
				failed[[2]string{id, label}] = true
			} else {
				m.sizes[label] += 0
			}
		}
	}
	for key := range failed {
		delete(volumes[key[0]].sizes, key[1])
	}
	rows := <-guests
	for _, m := range volumes {
		rows = append(rows, m)
	}
	return rows
}

func driveLetter(volume *uint16) string {
	var mounts [4096]uint16
	var length uint32
	if windows.GetVolumePathNamesForVolumeName(volume, &mounts[0], uint32(len(mounts)), &length) != nil {
		return ""
	}
	paths := mounts[:min(int(length), len(mounts))]
	for len(paths) > 0 && paths[0] != 0 {
		n := 0
		for n < len(paths) && paths[n] != 0 {
			n++
		}
		if n == 3 && paths[0] >= 'A' && paths[0] <= 'Z' && paths[1] == ':' && paths[2] == 92 {
			return string(rune(paths[0])) + ":"
		}
		paths = paths[min(n+1, len(paths)):]
	}
	return ""
}
func windowsVolumeID(path string) string {
	p, e := windows.UTF16PtrFromString(path)
	var mount, guid [windows.MAX_PATH]uint16
	if e != nil || windows.GetVolumePathName(p, &mount[0], uint32(len(mount))) != nil ||
		windows.GetVolumeNameForVolumeMountPoint(&mount[0], &guid[0], uint32(len(guid))) != nil {
		return ""
	}
	return volumeID("win", strings.ToLower(windows.UTF16ToString(guid[:])))
}

// Walk in batches so a single large module-cache directory cannot allocate an
// unbounded listing. Skip Windows junctions as well as symbolic links.
func treeSize(ctx context.Context, path string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	p, e := windows.UTF16PtrFromString(path)
	if e != nil {
		return 0, e
	}
	attrs, e := windows.GetFileAttributes(p)
	if e != nil {
		return 0, e
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return 0, nil
	}
	info, e := os.Lstat(path)
	if e != nil {
		return 0, e
	}
	if !info.IsDir() {
		if info.Mode().IsRegular() {
			return uint64(info.Size()), nil
		}
		return 0, nil
	}
	dir, e := os.Open(path)
	if e != nil {
		return 0, e
	}
	defer dir.Close()
	var sum uint64
	for {
		names, err := dir.Readdirnames(128)
		for _, name := range names {
			n, e := treeSize(ctx, filepath.Join(path, name))
			if e != nil {
				return 0, e
			}
			sum += n
		}
		if err == io.EOF {
			return sum, nil
		}
		if err != nil {
			return 0, err
		}
	}
}

func windowsStores() map[string][]string {
	home, _ := os.UserHomeDir()
	cache, _ := conf.DefaultCacheDir()
	stores := map[string][]string{}
	for _, name := range []string{"daemon.log", "daemon.log.1", "bashy-supervisord.log.1", "nodegc.log", "outpost.log", "supervisord.log", "bashy-supervisord.log", "upgrade.log", "scheduler.log", "cli-trace.log"} {
		stores["logs"] = append(stores["logs"], filepath.Join(cache, name))
	}
	for _, name := range []string{"post-install.json", "post-install-reboot.json", "post-install-reboot.pending.json"} {
		stores["evidence"] = append(stores["evidence"], filepath.Join(cache, name))
	}
	if pd := os.Getenv("ProgramData"); pd != "" {
		stores["evidence"] = append(stores["evidence"], filepath.Join(pd, "Outpost-ListenerAudit"))
	}
	spool := os.Getenv("BASHY_OTEL_SPOOL")
	if spool == "" {
		spool = filepath.Join(home, ".agents", "otel", "spool")
	}
	stores["otel"] = []string{spool}
	goPaths := goCacheSettings()
	build := goPaths["GOCACHE"]
	if build == "" {
		if dir, e := os.UserCacheDir(); e == nil {
			build = filepath.Join(dir, "go-build")
		}
	}
	mods := goPaths["GOMODCACHE"]
	if mods == "" {
		gp := filepath.SplitList(goPaths["GOPATH"])
		if len(gp) == 0 {
			gp = []string{filepath.Join(home, "go")}
		}
		mods = filepath.Join(gp[0], "pkg", "mod")
	}
	for label, path := range map[string]string{"go-build": build, "go-mod": mods} {
		if filepath.IsAbs(path) {
			if real, e := filepath.EvalSymlinks(path); e == nil {
				path = real
			}
			stores[label] = []string{path}
		}
	}
	return stores
}

func wslVolumes(ctx context.Context) []measurement {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Lxss`, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	defer key.Close()
	names, err := key.ReadSubKeyNames(maxVolumes)
	if err != nil && err != io.EOF {
		return nil
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return nil
	}
	exe := filepath.Join(system, "wsl.exe")
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	raw, err := probeOutput(listCtx, exe, "--list", "--running", "--quiet")
	cancel()
	if err != nil {
		return nil
	}
	running := runningDistros(raw)
	var rows []measurement
	for _, guid := range names {
		if ctx.Err() != nil || len(rows) >= maxVolumes {
			break
		}
		sub, err := registry.OpenKey(key, guid, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		name, _, err := sub.GetStringValue("DistributionName")
		sub.Close()
		if err != nil || !running[strings.ToLower(name)] {
			continue
		}
		sample, cancel := context.WithTimeout(ctx, 45*time.Second)
		display := name
		if !distroLabel.MatchString(display) {
			display = strings.Trim(guid, "{}")
		}
		if !distroLabel.MatchString(display) {
			cancel()
			continue
		}
		rows = append(rows, linuxMeasurements(sample, "wsl", strings.ToLower(guid), "WSL "+display, exe, "--distribution", name, "--exec", "sh", "-c", diskScript)...)
		cancel()
	}
	return rows
}

// go env -w stores literal key=value lines. The process environment wins.
func goCacheSettings() map[string]string {
	values := map[string]string{"GOCACHE": os.Getenv("GOCACHE"), "GOMODCACHE": os.Getenv("GOMODCACHE"), "GOPATH": os.Getenv("GOPATH")}
	path := os.Getenv("GOENV")
	if path == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			path = filepath.Join(dir, "go", "env")
		}
	}
	if path == "" || path == "off" {
		return values
	}
	f, err := os.Open(path)
	if err != nil {
		return values
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return values
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, val, ok := strings.Cut(strings.TrimSuffix(line, "\r"), "=")
		if current, known := values[key]; ok && known && current == "" {
			values[key] = val
		}
	}
	return values
}
