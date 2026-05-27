//go:build darwin

package osversion

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// detect on darwin uses sw_vers, which is in /usr/bin on every macOS
// since OS X 10. Three queries (-productName / -productVersion /
// -buildVersion) combined into one line:
//
//   "macOS 15.1.0 (24B83)"
//
// sw_vers is always available + fast (subprocess returns in <5ms on a
// typical Mac); no need to parse plists or read /System/Library.
func detect() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	name := swVers(ctx, "-productName")
	version := swVers(ctx, "-productVersion")
	build := swVers(ctx, "-buildVersion")
	if name == "" && version == "" {
		return ""
	}
	out := strings.TrimSpace(name + " " + version)
	if build != "" {
		out += " (" + build + ")"
	}
	return out
}

func swVers(ctx context.Context, flag string) string {
	out, err := exec.CommandContext(ctx, "sw_vers", flag).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
