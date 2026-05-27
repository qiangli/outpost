//go:build windows

package upgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SwapAtomic on Windows can't do the one-step rename Unix supports —
// the running .exe is held with an exclusive lock that rejects
// os.Rename(candidate → current) outright. But Windows *does* allow
// renaming a running .exe to a sibling name (the file stays in-use
// under the new path; the original path becomes free). The pattern:
//
//  1. Rename current → current+".replaced-<ts>". Frees the original
//     path. The running process keeps executing from the renamed file
//     just fine.
//  2. Rename candidate → current. Places the new binary at the path
//     that the next exec / service-restart will pick up.
//
// On any error in step 2 we rename .replaced back to current so the
// daemon isn't left binaryless. The .replaced-<ts> sibling file can't
// be deleted while the process is still running (Windows refuses
// unlink on in-use files); CleanupStaleSwaps drops it on the next
// daemon start after the prior PID has exited.
func SwapAtomic(current, candidate string) error {
	replaced := current + ".replaced-" + time.Now().UTC().Format("20060102-150405")
	if err := os.Rename(current, replaced); err != nil {
		return fmt.Errorf("rename old binary out of the way: %w", err)
	}
	if err := os.Rename(candidate, current); err != nil {
		// Roll back so we don't leave the daemon binaryless. If THIS
		// rename also fails, we're in trouble — surface the original
		// error since that's the actionable one.
		_ = os.Rename(replaced, current)
		return fmt.Errorf("rename candidate into place: %w", err)
	}
	return nil
}

// CleanupStaleSwaps removes any <current>.replaced-* siblings left
// behind by past upgrade runs. Safe to call multiple times; safe
// when no siblings exist. Called once at daemon startup so a fresh
// process tidies up after the prior generation exited.
func CleanupStaleSwaps(current string) {
	dir := filepath.Dir(current)
	prefix := filepath.Base(current) + ".replaced-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
