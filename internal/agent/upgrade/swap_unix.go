//go:build !windows

package upgrade

import "os"

// SwapAtomic puts `candidate` at `current`, replacing whatever was there.
// On Unix the kernel allows os.Rename over a file that's currently being
// executed — the running process keeps its file open via inode reference,
// the path is rebound to the new file, and the next exec from the path
// reads the new binary. Single rename, fully atomic.
//
// The current file is replaced; callers that want to retain the prior
// generation for rollback must do so BEFORE calling SwapAtomic (the
// Worker uses RetainPrevious to hardlink outpost.previous next to
// outpost first).
func SwapAtomic(current, candidate string) error {
	return os.Rename(candidate, current)
}

// CleanupStaleSwaps is a no-op on Unix — the atomic-rename path leaves
// no temp files behind. The function exists so cross-platform callers
// (main.go) don't need build tags.
func CleanupStaleSwaps(_ string) {}
