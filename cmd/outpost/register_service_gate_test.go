package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// TestActivatePairedServiceForPath_XDGScratchLeftUntouched verifies the
// regression: XDG_CONFIG_HOME=<scratch> must not touch the host service.
// Also covers default (installs) and --service override (installs despite scratch).
func TestActivatePairedServiceForPath_XDGScratchLeftUntouched(t *testing.T) {
	original := pairingServiceManager
	fake := &fakeUserServiceManager{}
	pairingServiceManager = fake
	t.Cleanup(func() { pairingServiceManager = original })

	// Use a temp HOME so DefaultConfigDirWithoutEnv is deterministic and
	// never collides with the real user's config.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	// Ensure XDG vars start clean.
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")

	scratch := t.TempDir()

	// Helper to compute a config path for a given XDG value.
	configPathFor := func(xdg string) string {
		t.Setenv("XDG_CONFIG_HOME", xdg)
		p, err := conf.DefaultConfigPath()
		if err != nil {
			t.Fatalf("DefaultConfigPath: %v", err)
		}
		return p
	}

	// 1) Default dir — should install.
	t.Run("default installs", func(t *testing.T) {
		p := configPathFor("")
		fake.installs = 0
		if err := activatePairedServiceForPath(p, false, false, false); err != nil {
			t.Fatalf("activate: %v", err)
		}
		if fake.installs != 1 {
			t.Fatalf("installs = %d, want 1 for default dir %q", fake.installs, p)
		}
		if !conf.IsDefaultConfigPath(p) {
			t.Fatalf("IsDefaultConfigPath(%q) = false, want true", p)
		}
		if !conf.ShouldTouchHostService(p, false) {
			t.Fatalf("ShouldTouchHostService(%q,false)=false, want true", p)
		}
	})

	// 2) Scratch XDG dir — must NOT install.
	t.Run("scratch does not install", func(t *testing.T) {
		p := configPathFor(scratch)
		if conf.IsDefaultConfigPath(p) {
			t.Fatalf("scratch path %q incorrectly considered default", p)
		}
		if conf.ShouldTouchHostService(p, false) {
			t.Fatalf("ShouldTouchHostService(%q,false)=true, want false", p)
		}
		fake.installs = 0
		if err := activatePairedServiceForPath(p, false, false, false); err != nil {
			t.Fatalf("activate: %v", err)
		}
		if fake.installs != 0 {
			t.Fatalf("installs = %d, want 0 for scratch dir %q", fake.installs, p)
		}
	})

	// 3) Scratch with --service — must install despite non-default.
	t.Run("scratch with --service installs", func(t *testing.T) {
		p := configPathFor(scratch)
		if !conf.ShouldTouchHostService(p, true) {
			t.Fatalf("ShouldTouchHostService(%q,true)=false, want true", p)
		}
		fake.installs = 0
		if err := activatePairedServiceForPath(p, false, false, true); err != nil {
			t.Fatalf("activate: %v", err)
		}
		if fake.installs != 1 {
			t.Fatalf("installs = %d, want 1 for --service on scratch %q", fake.installs, p)
		}
	})

	// 4) Explicit --out outside default — must not install without --service.
	t.Run("explicit out non-default without service", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")
		custom := filepath.Join(scratch, "custom", "agent.json")
		if conf.IsDefaultConfigPath(custom) {
			t.Fatalf("custom path %q incorrectly considered default", custom)
		}
		fake.installs = 0
		if err := activatePairedServiceForPath(custom, false, false, false); err != nil {
			t.Fatalf("activate: %v", err)
		}
		if fake.installs != 0 {
			t.Fatalf("installs = %d, want 0 for custom out %q", fake.installs, custom)
		}
	})

	// 5) no-service always wins, even on default.
	t.Run("no-service suppresses default", func(t *testing.T) {
		p := configPathFor("")
		fake.installs = 0
		if err := activatePairedServiceForPath(p, false, true, false); err != nil {
			t.Fatalf("activate: %v", err)
		}
		if fake.installs != 0 {
			t.Fatalf("installs = %d, want 0 with --no-service", fake.installs)
		}
	})
}

// TestRegisterServiceFlags ensures the cobra flags exist.
func TestRegisterServiceFlags(t *testing.T) {
	cmd := registerCmd()
	if cmd.Flags().Lookup("service") == nil {
		t.Fatal("--service flag not registered on registerCmd")
	}
	if cmd.Flags().Lookup("no-service") != nil {
		t.Fatal("--no-service was removed (one lifecycle, no opt-out)")
	}
}

// TestConfIsDefaultHelpers covers the factored comparison directly.
func TestConfIsDefaultHelpers(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_CONFIG_HOME", "")

	defDir, err := conf.DefaultConfigDirWithoutEnv()
	if err != nil {
		t.Fatal(err)
	}
	defPath := filepath.Join(defDir, "agent.json")
	if !conf.IsDefaultConfigDir(defDir) {
		t.Fatalf("IsDefaultConfigDir(%q) false, want true", defDir)
	}
	if !conf.IsDefaultConfigPath(defPath) {
		t.Fatalf("IsDefaultConfigPath(%q) false, want true", defPath)
	}
	if !conf.ShouldTouchHostService(defPath, false) {
		t.Fatalf("ShouldTouchHostService(%q,false) false, want true", defPath)
	}

	scratch := filepath.Join(t.TempDir(), "matrix", "agent.json")
	if conf.IsDefaultConfigPath(scratch) {
		t.Fatalf("scratch %q incorrectly default", scratch)
	}
	if conf.ShouldTouchHostService(scratch, false) {
		t.Fatalf("scratch ShouldTouchHostService false expected")
	}
	if !conf.ShouldTouchHostService(scratch, true) {
		t.Fatalf("scratch with --service ShouldTouchHostService true expected")
	}
	if !conf.ShouldTouchHostService("", true) {
		t.Fatalf("empty path with --service should be true")
	}
	// Also verify HOME isolation: different HOME yields different default.
	otherHome := t.TempDir()
	otherDef := filepath.Join(otherHome, ".config", "matrix")
	if conf.IsDefaultConfigDir(otherDef) {
		// This should be false because HOME is still tmpHome, not otherHome.
		t.Fatalf("IsDefaultConfigDir(%q) true but HOME is %q, want false", otherDef, tmpHome)
	}
	// Ensure IsDefaultConfigDir is cleaned: trailing slash should still match.
	cleaned := defDir + "/"
	if !conf.IsDefaultConfigDir(cleaned) {
		t.Fatalf("IsDefaultConfigDir with trailing slash %q false, want true", cleaned)
	}
	_ = os.MkdirAll // keep import used
}
