package runtime

import (
	"fmt"
	"os"
	"path/filepath"
)

// ensureBundledPodmanConf points a binmgr-cached podman at the helper
// binaries shipped inside its own tarball (<root>/bin/podman, <root>/lib/podman)
// via CONTAINERS_CONF_OVERRIDE. A value the operator already set is respected.
func ensureBundledPodmanConf(bin string) {
	if os.Getenv("CONTAINERS_CONF_OVERRIDE") != "" {
		return
	}
	binDir := filepath.Dir(bin)
	root := filepath.Clean(filepath.Join(binDir, ".."))
	libDir := filepath.Join(root, "lib", "podman")
	if st, err := os.Stat(libDir); err != nil || !st.IsDir() {
		return
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return
	}
	dir := filepath.Join(cache, "outpost")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "containers.conf")
	want := fmt.Sprintf(`[engine]
conmon_path = [%[1]q]
helper_binaries_dir = [%[2]q, %[3]q, %[4]q]

[engine.runtimes]
crun = [%[5]q]
runc = [%[6]q]
`, filepath.Join(libDir, "conmon"), libDir, filepath.Join(root, "libexec", "podman"), binDir,
		filepath.Join(binDir, "crun"), filepath.Join(binDir, "runc"))
	if cur, err := os.ReadFile(path); err != nil || string(cur) != want {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(want), 0o644); err != nil {
			return
		}
		if err := os.Rename(tmp, path); err != nil {
			return
		}
	}
	os.Setenv("CONTAINERS_CONF_OVERRIDE", path)
}
