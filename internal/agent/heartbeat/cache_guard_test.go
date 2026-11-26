//go:build !windows

package heartbeat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func cacheFixtureTools(t *testing.T) string {
	t.Helper()
	tools := t.TempDir()
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	return tools
}

func cacheFixtureCommand(t *testing.T, tools, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentCacheAdmission(t *testing.T) {
	root := t.TempDir()
	volume := filepath.Join(root, "cache")
	build := filepath.Join(volume, "build")
	modules := filepath.Join(volume, "modules")
	for _, dir := range []string{build, modules} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join("..", "..", "..", "scripts", "check-go-cache-space.sh")
	t.Setenv("GOCACHE", build)
	t.Setenv("GOMODCACHE", modules)
	t.Setenv("GO_CACHE_VOLUME_ROOT", volume)
	tools := cacheFixtureTools(t)
	cacheFixtureCommand(t, tools, "realpath", "printf '%s\\n' \"$3\"")
	cacheFixtureCommand(t, tools, "mountpoint", "exit 0")
	cacheFixtureCommand(t, tools, "findmnt", "printf '/\\n'")
	cacheFixtureCommand(t, tools, "stat", `
if [ "$1" = -f ]; then
    printf '%s\n' "$CACHE_FIXTURE_STAT"
else
    case "$4" in
        "$GO_CACHE_VOLUME_ROOT"|"$GO_CACHE_VOLUME_ROOT"/*) printf '2\n' ;;
        *) printf '1\n' ;;
    esac
fi`)
	marker := filepath.Join(modules, "preserved")
	if err := os.WriteFile(marker, []byte("cache"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, stat, ceiling, want string
		ok                        bool
	}{
		{"empty stat", "", "100", "filesystem statistics", false},
		{"below threshold", "1 100 6", "100", "available_bytes=6", true},
		{"at 95", "1 100 5", "100", "defer new cache writes", false},
		{"oversized filesystem", "4 100 10", "399", "exceeds configured ceiling", false},
		{"missing ceiling", "4 100 10", "", "configure GO_CACHE_VOLUME_MAX_BYTES", false},
		{"disabled ceiling", "4 100 10", "0", "positive, exact byte count", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GO_CACHE_VOLUME_MAX_BYTES", tt.ceiling)
			t.Setenv("CACHE_FIXTURE_STAT", tt.stat)
			raw, err := exec.Command("sh", script).CombinedOutput()
			if (err == nil) != tt.ok || !strings.Contains(string(raw), tt.want) {
				t.Fatalf("%s: %v", raw, err)
			}
			if tt.name == "at 95" || tt.name == "missing ceiling" {
				t.Setenv("WSL_DISTRO_NAME", "Fixture")
				t.Setenv("OUT_DIR", filepath.Join(root, "build-output"))
				raw, err := exec.Command("bash", filepath.Join(filepath.Dir(script), "build.sh")).CombinedOutput()
				if err == nil || !strings.Contains(string(raw), tt.want) {
					t.Fatalf("WSL build bypassed cache admission: %s, %v", raw, err)
				}
				if _, err := os.Stat(os.Getenv("OUT_DIR")); !os.IsNotExist(err) {
					t.Fatal("build started after cache admission was refused")
				}
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "cache" {
				t.Fatal("check changed the cache")
			}
		})
	}
	t.Run("missing mount", func(t *testing.T) {
		t.Setenv("GO_CACHE_VOLUME_MAX_BYTES", "100")
		t.Setenv("GO_CACHE_VOLUME_ROOT", "")
		if raw, err := exec.Command("sh", script).CombinedOutput(); err == nil ||
			!strings.Contains(string(raw), "configure GO_CACHE_VOLUME_ROOT") {
			t.Fatalf("mount was optional: %s, %v", raw, err)
		}
	})
	t.Run("cache outside mount", func(t *testing.T) {
		t.Setenv("GO_CACHE_VOLUME_MAX_BYTES", "100")
		t.Setenv("GOCACHE", root)
		if raw, err := exec.Command("sh", script).CombinedOutput(); err == nil ||
			!strings.Contains(string(raw), "go-build must be inside") {
			t.Fatalf("out-of-mount cache was admitted: %s, %v", raw, err)
		}
	})
}

func TestPersistentCacheKernelCeiling(t *testing.T) {
	volume := os.Getenv("OUTPOST_CACHE_FIXTURE_VOLUME")
	if volume == "" {
		t.Skip("requires a dedicated 8 MiB tmpfs fixture mount")
	}
	if raw, err := exec.Command("findmnt", "--noheadings", "--output", "FSTYPE", "--mountpoint", volume).Output(); err != nil ||
		strings.TrimSpace(string(raw)) != "tmpfs" {
		t.Fatalf("fixture volume must be a tmpfs mount: %s, %v", raw, err)
	}
	root, err := os.MkdirTemp(volume, "cache-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	build := filepath.Join(root, "build")
	modules := filepath.Join(root, "modules")
	for _, dir := range []string{build, modules} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	preserved := filepath.Join(modules, "existing-module")
	if err := os.WriteFile(preserved, []byte("persistent fixture"), 0600); err != nil {
		t.Fatal(err)
	}

	stage := t.TempDir()
	attempt := filepath.Join(stage, "compiler-started")
	t.Setenv("WSL_DISTRO_NAME", "Fixture")
	t.Setenv("GO_CACHE_VOLUME_ROOT", volume)
	t.Setenv("GO_CACHE_VOLUME_MAX_BYTES", "")
	t.Setenv("GOCACHE", build)
	t.Setenv("GOMODCACHE", modules)
	t.Setenv("OUT_DIR", filepath.Join(stage, "bin"))
	t.Setenv("CACHE_FIXTURE_ATTEMPT", attempt)
	tools := cacheFixtureTools(t)
	cacheFixtureCommand(t, tools, "go", `
set -eu
export LC_ALL=C
[ "$1" = build ]
printf 'admitted\n' > "$CACHE_FIXTURE_ATTEMPT"
dd if=/dev/zero of="$GOCACHE/crossing" bs=1048576 count=16`)
	script := filepath.Join("..", "..", "..", "scripts", "build.sh")
	raw, err := exec.Command("bash", script).CombinedOutput()
	if err == nil || !strings.Contains(string(raw), "configure GO_CACHE_VOLUME_MAX_BYTES") {
		t.Fatalf("missing ceiling did not refuse build: %s, %v", raw, err)
	}
	if _, err := os.Stat(attempt); !os.IsNotExist(err) {
		t.Fatal("compiler started without a configured ceiling")
	}

	const ceiling = 8 << 20
	t.Setenv("GO_CACHE_VOLUME_MAX_BYTES", "8388608")
	raw, err = exec.Command("bash", script).CombinedOutput()
	if err == nil || !strings.Contains(string(raw), "total_bytes=8388608") ||
		!strings.Contains(string(raw), "No space left on device") {
		t.Fatalf("admitted build did not hit the filesystem ceiling: %s, %v", raw, err)
	}
	if data, err := os.ReadFile(attempt); err != nil || string(data) != "admitted\n" {
		t.Fatalf("compiler did not reach the cache write: %q, %v", data, err)
	}
	item, err := os.Stat(filepath.Join(build, "crossing"))
	if err != nil || item.Size() <= 0 || item.Size() >= ceiling {
		t.Fatalf("expected a partially written file below the shared ceiling: %v, %v", item, err)
	}
	if data, err := os.ReadFile(preserved); err != nil || string(data) != "persistent fixture" {
		t.Fatalf("prior cache contents changed: %q, %v", data, err)
	}
	t.Log("missing configuration refused the build; the admitted write received ENOSPC on the 8 MiB mount and preserved the existing module")
}
