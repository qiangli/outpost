//go:build !windows

package heartbeat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersistentCacheAdmission(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join("..", "..", "..", "scripts", "check-go-cache-space.sh")
	t.Setenv("GOCACHE", root)
	t.Setenv("GOMODCACHE", root)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	marker := filepath.Join(root, "preserved")
	if err := os.WriteFile(marker, []byte("cache"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, stat, ceiling string
		ok                  bool
	}{
		{"empty stat", "", "100", false},
		{"below", "1 100 6", "100", true},
		{"at 95", "1 100 5", "100", false},
		{"oversized filesystem", "4 100 10", "399", false},
		{"no ceiling configured", "4 100 10", "0", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GO_CACHE_VOLUME_MAX_BYTES", tt.ceiling)
			if err := os.WriteFile(filepath.Join(root, "stat"), []byte("#!/bin/sh\necho '"+tt.stat+"'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			raw, err := exec.Command("sh", script).CombinedOutput()
			if (err == nil) != tt.ok || !strings.Contains(string(raw), "go-mod") {
				t.Fatalf("%s: %v", raw, err)
			}
			if tt.name == "at 95" {
				t.Setenv("WSL_DISTRO_NAME", "Fixture")
				t.Setenv("OUT_DIR", filepath.Join(root, "build-output"))
				raw, err := exec.Command("bash", filepath.Join(filepath.Dir(script), "build.sh")).CombinedOutput()
				if err == nil || !strings.Contains(string(raw), "defer new cache writes") {
					t.Fatalf("WSL build bypassed cache threshold: %s, %v", raw, err)
				}
				if _, err := os.Stat(os.Getenv("OUT_DIR")); !os.IsNotExist(err) {
					t.Fatal("build started while cache writes were refused")
				}
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "cache" {
				t.Fatal("check changed the cache")
			}
		})
	}
}
