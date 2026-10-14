package main

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// The shard ring must no longer skip Windows: any platform with a signed
// prima engine provisions through binmgr and can take a rank. The gate
// follows the Yoke pin — not the leader's own OS — and fails open when the
// status board has no platform for a peer.
func TestKeepEnginePeerFollowsSignedRelease(t *testing.T) {
	released := map[string]bool{"linux/amd64": true, "windows/amd64": true}
	cases := map[string]struct {
		platform string
		keep     bool
	}{
		"windows rank":       {"windows/amd64", true},
		"same-platform rank": {"linux/amd64", true},
		"unreleased rank":    {"freebsd/amd64", false},
		"unknown platform":   {"", true},
	}
	for name, c := range cases {
		if got := keepEnginePeer(c.platform, released); got != c.keep {
			t.Errorf("%s: keepEnginePeer(%q) = %v, want %v", name, c.platform, got, c.keep)
		}
	}
}

// The production gate set is exactly the pinned Prima release: no
// hand-maintained platform list to drift from the pin.
func TestPrimaEnginePlatformsMatchPin(t *testing.T) {
	platforms := primaEnginePlatforms()
	if len(platforms) == 0 {
		t.Fatal("no engine platforms from the pin")
	}
	for p := range binmgr.PrimaTool().Assets {
		if !platforms[p] {
			t.Errorf("pinned platform %s missing from the engine gate", p)
		}
	}
	for p := range platforms {
		parts := strings.Split(p, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			t.Errorf("engine platform %q is not goos/goarch", p)
		}
		if _, ok := binmgr.PrimaTool().Assets[p]; !ok {
			t.Errorf("engine platform %s is not in the pinned release", p)
		}
	}
	// The gate defers to the pin for Windows: whatever the pinned release
	// ships determines whether a Windows peer takes a rank.
	wantWindows := platforms["windows/amd64"]
	if got := keepEnginePeer("windows/amd64", platforms); got != wantWindows {
		t.Errorf("windows gate = %v, pinned release ships windows/amd64 = %v", got, wantWindows)
	}
}
