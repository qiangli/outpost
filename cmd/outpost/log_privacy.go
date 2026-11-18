package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// Existing installations can retain public directory and log permissions.
// Tighten them before any daemon, supervisor or trace writer appends again.
func protectDaemonLogs() error {
	dir, err := conf.DefaultCacheDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create private log directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("protect log directory: %w", err)
	}
	for _, name := range []string{"daemon.log", "bashy-supervisord.log", "supervisord.log", "outpost.log", "cli-trace.log"} {
		path := filepath.Join(dir, name)
		if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("protect %s: %w", name, err)
		}
	}
	return nil
}
