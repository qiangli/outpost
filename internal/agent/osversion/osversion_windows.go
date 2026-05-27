//go:build windows

package osversion

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// detect on windows shells out to `cmd /c ver`, which prints something
// like:
//
//   Microsoft Windows [Version 10.0.26100.2034]
//
// Cleaner alternatives (registry: SOFTWARE\Microsoft\Windows NT\
// CurrentVersion or RtlGetVersion) involve more LazyDLL ceremony for
// a string we render once per host. Fast enough — the subprocess
// returns in <50ms and we cache the result.
func detect() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "cmd", "/c", "ver").Output()
	if err != nil {
		return ""
	}
	// Strip newlines + brackets; "Microsoft Windows [Version X]"
	// becomes "Microsoft Windows X" for tighter UI display.
	s := strings.TrimSpace(string(out))
	s = strings.ReplaceAll(s, "[Version ", "")
	s = strings.TrimSuffix(s, "]")
	return s
}
