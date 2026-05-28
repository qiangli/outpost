package ycode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Start spawns `ycode serve` as a detached background process, then
// waits up to readyTimeout for the manifest to appear and the api
// endpoint to become reachable. Returns the Info snapshot of the
// freshly-started process on success.
//
// Idempotent against concurrent callers: if Detect reports a Running
// state, Start returns immediately with that Info. Two parallel
// outpost restarts that both call Start() do NOT produce two ycode
// instances — the second one sees the first's manifest go live and
// returns without spawning. ycode's own serve command refuses to
// start a second instance if the manifest is locked.
//
// Stale-manifest handling: when Detect reports StateStaleManifest,
// we delete the manifest file before spawning so ycode's own
// already-running detection (which keys off the file) doesn't trip.
func Start(ctx context.Context) (Info, error) {
	info := Detect()
	switch info.State {
	case StateRunning:
		return info, nil
	case StateNotInstalled:
		return info, fmt.Errorf("ycode binary not found; install from %s", info.DownloadURL)
	case StateStaleManifest:
		// Delete the stale manifest so ycode's own lockfile check
		// doesn't refuse the start. Idempotent on missing.
		_ = os.Remove(info.ManifestPath)
	}
	if info.BinaryPath == "" {
		return info, errors.New("ycode binary not found")
	}

	if err := startDetached(info.BinaryPath); err != nil {
		return info, fmt.Errorf("start ycode serve: %w", err)
	}

	// Poll for liveness. The manifest is written near the end of
	// ycode's serve bootstrap (after the HTTP listener is up), so a
	// successful httpAlive(manifest.api) is the right "I'm ready"
	// signal. We don't watch the binary's stdout — the process is
	// detached.
	return waitForReady(ctx, 30*time.Second)
}

// Stop is intentionally NOT implemented in v1. ycode serve is a
// daemon the user invoked; outpost shouldn't kill it just because
// outpost is shutting down — the user may want their inference
// service to outlive outpost restarts. A future "Stop" affordance
// in the admin UI would dial ycode's own /shutdown endpoint, not
// SIGTERM the process directly.

// waitForReady polls Detect every 250 ms until State==Running or the
// timeout elapses. Returns the final Info regardless — the caller
// can inspect Info.State to decide whether the start succeeded.
func waitForReady(ctx context.Context, timeout time.Duration) (Info, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var last Info
	for {
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-ticker.C:
			last = Detect()
			if last.State == StateRunning {
				return last, nil
			}
			if time.Now().After(deadline) {
				return last, fmt.Errorf("ycode serve did not become ready within %s (state=%s)", timeout, last.State)
			}
		}
	}
}

// logFilePath is where the detached ycode serve writes stdout +
// stderr. Same location across platforms (under the outpost cache
// dir) so an operator can `tail -F` it to debug startup issues.
// Caller responsibility to ensure the dir exists before spawn.
func logFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "outpost", "ycode-serve.log")
}

// openOrCreateLog opens the log file for append, creating it (and
// any parent dirs) if needed. Returns nil + nil on filesystem
// failure — the caller spawns ycode with /dev/null as stdout in that
// case, so a missing log file is non-fatal.
func openOrCreateLog() *os.File {
	p := logFilePath()
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}

// ycodeServeArgs is the argv ycode serve takes. Stable across
// versions; new flags are opt-in. We pass nothing so ycode picks
// its own defaults (manifest under $HOME/.agents/ycode, default
// port from $YCODE_PORT or 31415).
func ycodeServeArgs() []string {
	return []string{"serve"}
}

// startDetached is platform-specific (see lifecycle_unix.go and
// lifecycle_windows.go). On Unix, setsid + Setpgid makes the child
// its own session leader so it survives the parent's exit. On
// Windows, CREATE_NEW_PROCESS_GROUP achieves the same.

// nopExec is the no-op exec.Cmd that the test path uses when we want
// to assert "Start would have spawned a process" without actually
// doing so. Tests inject this via the `spawn` package variable.
var spawn = realSpawn

// realSpawn does the actual exec. Replaced in tests via the spawn
// variable above so the detached-process plumbing can be exercised
// without forking a real subprocess.
func realSpawn(bin string) error {
	cmd := exec.Command(bin, ycodeServeArgs()...)
	if log := openOrCreateLog(); log != nil {
		cmd.Stdout = log
		cmd.Stderr = log
		// Close our handle once cmd.Start returns — the child has
		// its own fd via dup2. Don't defer (the cmd.Start may run
		// concurrently with our return); rely on GC.
	}
	platformDetach(cmd)
	return cmd.Start()
}
