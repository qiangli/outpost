package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// stopintent.go — the stop-vs-kill contract.
//
// Only `outpost stop` writes a stop-intent marker. The supervisor parks the
// daemon while the marker (or the disable marker) is present and restarts it
// after ANY other exit — kill -9, crash, clean exit — with backoff. The
// stop-intent clears at every supervisor boot (and on an explicit
// `outpost start`); the disable marker persists until `outpost enable`.
//
//   - stop-intent: <cache>/supervisord.stop — one-shot "stay down".
//   - disable:     <config>/supervisord.disabled — persistent "stay down
//     across boots" (the `outpost disable` half).
//
// Both live next to the pidfile/config the supervisor already owns, so
// Temp HOME/XDG redirection in tests isolates them and no real service
// is ever touched.

const (
	stopIntentFileName    = "supervisord.stop"
	disableMarkerFileName = "supervisord.disabled"
)

// stopIntentPath is the one-shot marker `outpost stop` writes.
func stopIntentPath() (string, error) {
	dir, err := conf.DefaultCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, stopIntentFileName), nil
}

// disableMarkerPath is the persistent marker `outpost disable` writes.
// It lives in the config dir (durable operator intent), not the cache.
func disableMarkerPath() (string, error) {
	dir, err := conf.DefaultConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, disableMarkerFileName), nil
}

func markerPresent(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// daemonShouldRun is the supervisor's ShouldRun gate for the daemon
// program: hold the daemon down only while an explicit stop or disable
// intent is on disk. Unreadable state fails OPEN (run) — a permissions
// hiccup must never wedge the daemon down.
func daemonShouldRun() bool {
	if p, err := stopIntentPath(); err == nil && markerPresent(p) {
		return false
	}
	if p, err := disableMarkerPath(); err == nil && markerPresent(p) {
		return false
	}
	return true
}

// writeStopIntent records the one-shot stop intent. Failing to write it
// is fatal to the caller: without the marker the supervisor would
// immediately restart the daemon `outpost stop` just signaled.
func writeStopIntent() error {
	p, err := stopIntentPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("prepare stop-intent dir: %w", err)
	}
	if err := os.WriteFile(p, []byte("stopped by `outpost stop`\n"), 0o600); err != nil {
		return fmt.Errorf("write stop intent: %w", err)
	}
	return nil
}

// clearStopIntent drops the one-shot intent: supervisor boot and an
// explicit `outpost start` both mean "run again". Best-effort — a
// missing file is the common case.
func clearStopIntent() {
	if p, err := stopIntentPath(); err == nil {
		_ = os.Remove(p)
	}
}

func writeDisableMarker() error {
	p, err := disableMarkerPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("prepare disable-marker dir: %w", err)
	}
	if err := os.WriteFile(p, []byte("disabled by `outpost disable`\n"), 0o600); err != nil {
		return fmt.Errorf("write disable marker: %w", err)
	}
	return nil
}

func clearDisableMarker() {
	if p, err := disableMarkerPath(); err == nil {
		_ = os.Remove(p)
	}
}

// disabledByMarker reports a persistent `outpost disable` intent.
func disabledByMarker() bool {
	p, err := disableMarkerPath()
	return err == nil && markerPresent(p)
}

// disableCmd implements `outpost disable`: persist "stay down across
// boots" and stop the daemon now. Unlike `stop` (one-shot, cleared at
// boot), the supervisor keeps the daemon parked until `outpost enable`.
func disableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Stop the daemon and keep it down across boots (cleared by `outpost enable`)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := writeDisableMarker(); err != nil {
				return err
			}
			if err := runStop(cmd); err != nil {
				return err
			}
			fmt.Println("Disabled — the supervisor will keep the daemon down until `outpost enable`.")
			return nil
		},
	}
}

// enableCmd implements `outpost enable`: clear stop + disable intents and
// make sure the daemon is running again — via the supervisor when one is
// installed, directly otherwise.
func enableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable",
		Short: "Clear stop/disable intents and start the daemon (via the supervisor when installed)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			clearStopIntent()
			clearDisableMarker()
			if daemonRunning() {
				fmt.Println("Outpost is already running.")
				return nil
			}
			if _, found, _ := discoverInit(); found {
				deadline := time.Now().Add(15 * time.Second)
				for time.Now().Before(deadline) {
					if daemonRunning() {
						fmt.Println("Enabled — the supervisor restarted the daemon.")
						return nil
					}
					time.Sleep(500 * time.Millisecond)
				}
				return errors.New("enabled, but the daemon did not come back under the supervisor — check `outpost supervisord status` and the daemon log")
			}
			// No supervisor: start the daemon directly, like register does.
			return execSelfStart()
		},
	}
}
