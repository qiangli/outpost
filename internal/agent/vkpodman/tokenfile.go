package vkpodman

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// WriteTokenFile atomically writes the bearer token to path, creating
// parent directories as needed. mode 0600 because the file is a live
// SA credential — anyone with read access to it has the same kube
// powers as the outpost.
//
// Atomic: write to <path>.tmp + rename. client-go's BearerTokenFile
// transport re-reads the file on its own schedule (every minute by
// default), and rename is the safe way to swap the contents without
// it seeing a partial write.
func WriteTokenFile(path, token string) error {
	if path == "" {
		return fmt.Errorf("vkpodman: empty token-file path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("vkpodman: mkdir token-file dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token), 0o600); err != nil {
		return fmt.Errorf("vkpodman: write token-file tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("vkpodman: rename token-file: %w", err)
	}
	return nil
}

// DefaultTokenFilePath returns the canonical path for the persisted
// SA bearer token: conf.DefaultCacheDir()/cluster-token (i.e.
// ~/.cache/outpost/cluster-token on Linux+macOS, %USERPROFILE%\.cache\
// outpost\cluster-token on Windows). Sharing the outpost cache dir
// with the rest of the agent's runtime state (pidfile, logs) keeps
// related state in one place — and the file mode 0600 stops it
// leaking even when the user's cache dir is world-readable.
func DefaultTokenFilePath() (string, error) {
	base, err := conf.DefaultCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "cluster-token"), nil
}
