package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureBundledPodmanConf(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "lib", "podman")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	t.Setenv("CONTAINERS_CONF_OVERRIDE", "")
	bin := filepath.Join(root, "bin", "podman")

	ensureBundledPodmanConf(bin)
	conf := os.Getenv("CONTAINERS_CONF_OVERRIDE")
	if conf == "" {
		t.Fatal("CONTAINERS_CONF_OVERRIDE not set")
	}
	b, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		`conmon_path = ["` + lib + `/conmon"]`,
		`helper_binaries_dir = ["` + lib + `"]`,
		`crun = ["` + lib + `/crun"]`,
		`runc = ["` + lib + `/runc"]`,
		"[engine.runtimes]",
	} {
		if !strings.Contains(string(b), w) {
			t.Errorf("conf missing %q:\n%s", w, b)
		}
	}

	t.Setenv("CONTAINERS_CONF_OVERRIDE", "/operator/set.conf")
	ensureBundledPodmanConf(bin)
	if got := os.Getenv("CONTAINERS_CONF_OVERRIDE"); got != "/operator/set.conf" {
		t.Errorf("operator value overwritten: %s", got)
	}

	t.Setenv("CONTAINERS_CONF_OVERRIDE", "")
	bare := filepath.Join(t.TempDir(), "bin", "podman")
	ensureBundledPodmanConf(bare)
	if os.Getenv("CONTAINERS_CONF_OVERRIDE") != "" {
		t.Error("set without lib/podman")
	}
}
