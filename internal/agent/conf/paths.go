package conf

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Canonical config / cache locations. Outpost is a terminal-first
// tool (CLI + daemon + headless MCP server); its files belong where
// dev tools live, which on macOS and Windows is NOT what
// `os.UserConfigDir()` / `os.UserCacheDir()` return.
//
//   - Go's os.UserConfigDir() on macOS  → ~/Library/Application Support
//   - Go's os.UserConfigDir() on Windows → %AppData%
//   - Go's os.UserCacheDir() on macOS   → ~/Library/Caches
//
// Those locations are conventional for GUI apps but make outpost's
// state hard to find for operators who live in a shell. Tools like
// `gh`, `kubectl`, `helm`, and `claude` all stash config under
// `~/.config/<app>/` regardless of platform; outpost follows the
// same path semantics across Linux, macOS, and Windows (where it
// resolves to `C:\Users\<user>\.config\matrix\`).
//
// $XDG_CONFIG_HOME / $XDG_CACHE_HOME, when set, win over the
// `$HOME/.config` / `$HOME/.cache` defaults — that's the standard XDG
// override and lets sandboxed runs point everything at a tempdir.

// DefaultConfigPath returns the canonical agent.json location.
func DefaultConfigPath() (string, error) {
	dir, err := DefaultConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent.json"), nil
}

// DefaultConfigDir returns the canonical parent directory of
// agent.json (and the SSH host key). Caller is responsible for
// MkdirAll.
func DefaultConfigDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "matrix"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "matrix"), nil
}

// DefaultCacheDir returns the canonical outpost cache directory
// (pidfile, log, session/outbound cookies, jobs, cluster token).
func DefaultCacheDir() (string, error) {
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, "outpost"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "outpost"), nil
}

// LegacyConfigPath returns where outpost stored agent.json before the
// XDG migration — Go's `os.UserConfigDir()/matrix/agent.json`. On
// Linux without XDG_CONFIG_HOME this equals DefaultConfigPath (no
// migration needed); on macOS/Windows it's a separate location.
func LegacyConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "matrix", "agent.json"), nil
}

// LegacyCacheDir returns where outpost stored cache files pre-migration
// — Go's `os.UserCacheDir()/outpost`.
func LegacyCacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "outpost"), nil
}

// ResolveConfigPath returns the canonical agent.json path, migrating
// from the legacy os.UserConfigDir() location if necessary.
//
// Behavior matrix:
//
//	canonical exists, legacy doesn't  → use canonical (steady state)
//	canonical doesn't, legacy exists  → rename legacy → canonical,
//	                                    also migrate ssh_host_ed25519
//	both exist                        → use whichever has the later
//	                                    mtime; rename the loser to
//	                                    *.bak.<unix-ts> so nothing
//	                                    is silently lost
//	neither exists                    → use canonical (first boot)
//
// Idempotent — second call is a stat-only no-op.
func ResolveConfigPath() (string, error) {
	canonical, err := DefaultConfigPath()
	if err != nil {
		return "", err
	}
	legacy, err := LegacyConfigPath()
	if err != nil {
		return canonical, nil //nolint:nilerr — if we can't resolve legacy, just use canonical
	}
	if legacy == canonical {
		return canonical, nil
	}
	canonicalInfo, canonicalErr := os.Stat(canonical)
	legacyInfo, legacyErr := os.Stat(legacy)
	switch {
	case canonicalErr == nil && legacyErr != nil:
		// Only canonical — steady state.
		return canonical, nil
	case canonicalErr != nil && legacyErr == nil:
		// Only legacy — auto-migrate.
		if err := migrateConfigFiles(legacy, canonical); err != nil {
			return "", err
		}
		slog.Info("conf: migrated agent.json from legacy location",
			"from", legacy, "to", canonical)
		return canonical, nil
	case canonicalErr == nil && legacyErr == nil:
		// Both exist — keep the newer one, back up the older.
		if legacyInfo.ModTime().After(canonicalInfo.ModTime()) {
			backup := canonical + suffixBak()
			if err := os.Rename(canonical, backup); err != nil {
				return "", fmt.Errorf("back up stale canonical agent.json: %w", err)
			}
			if err := migrateConfigFiles(legacy, canonical); err != nil {
				return "", err
			}
			slog.Warn("conf: agent.json present at both canonical and legacy paths; legacy was newer — using it, backed up canonical",
				"backup", backup, "canonical", canonical)
		} else {
			backup := legacy + suffixBak()
			if err := os.Rename(legacy, backup); err != nil {
				return "", fmt.Errorf("back up stale legacy agent.json: %w", err)
			}
			slog.Warn("conf: agent.json present at both canonical and legacy paths; canonical was newer — kept it, backed up legacy",
				"backup", backup, "canonical", canonical)
		}
		return canonical, nil
	default:
		// Neither exists — first boot.
		return canonical, nil
	}
}

// ResolveCacheDir returns the canonical cache directory, migrating
// the entire legacy directory (pidfile, log, sessions/, outbounds/,
// jobs/, cluster-token) on first call when the canonical location is
// empty. Best-effort: errors are logged but don't fail the boot
// path — cache files are transient and rebuild themselves if lost.
func ResolveCacheDir() (string, error) {
	canonical, err := DefaultCacheDir()
	if err != nil {
		return "", err
	}
	legacy, err := LegacyCacheDir()
	if err != nil {
		return canonical, nil //nolint:nilerr
	}
	if legacy == canonical {
		if err := os.MkdirAll(canonical, 0o700); err != nil {
			return "", err
		}
		return canonical, nil
	}
	_, canonicalErr := os.Stat(canonical)
	_, legacyErr := os.Stat(legacy)
	switch {
	case canonicalErr == nil:
		// Canonical already populated; legacy (if any) is stale.
		// Don't auto-migrate cache contents when both exist — too
		// many tiny files, race-prone. Operator can `rm -rf legacy`
		// once they're satisfied the move worked.
		return canonical, os.MkdirAll(canonical, 0o700)
	case legacyErr == nil:
		// Only legacy — atomic rename of the whole dir.
		if err := os.MkdirAll(filepath.Dir(canonical), 0o700); err != nil {
			return "", err
		}
		if err := os.Rename(legacy, canonical); err != nil {
			// cross-volume fallback isn't worth the complexity for
			// cache contents — log and fall through to creating a
			// fresh empty cache dir.
			slog.Warn("conf: could not rename legacy cache dir; leaving it in place — operator can move manually",
				"from", legacy, "to", canonical, "err", err)
			return canonical, os.MkdirAll(canonical, 0o700)
		}
		slog.Info("conf: migrated cache directory from legacy location",
			"from", legacy, "to", canonical)
		return canonical, nil
	default:
		// Neither exists — first boot.
		return canonical, os.MkdirAll(canonical, 0o700)
	}
}

// migrateConfigFiles renames the agent.json and the sibling SSH host
// key from `legacy` to `canonical`, creating the canonical parent
// directory as needed. Falls back to copy+remove when the two paths
// straddle a filesystem boundary.
func migrateConfigFiles(legacy, canonical string) error {
	if err := os.MkdirAll(filepath.Dir(canonical), 0o700); err != nil {
		return fmt.Errorf("mkdir canonical parent: %w", err)
	}
	if err := os.Rename(legacy, canonical); err != nil {
		// Cross-volume? Copy + remove.
		data, rerr := os.ReadFile(legacy)
		if rerr != nil {
			return fmt.Errorf("read legacy: %w", rerr)
		}
		if werr := os.WriteFile(canonical, data, 0o600); werr != nil {
			return fmt.Errorf("write canonical: %w", werr)
		}
		_ = os.Remove(legacy)
	}
	// Also move the sibling SSH host key (`ssh_host_ed25519`) so
	// clients' known_hosts entries stay valid after the migration.
	legacyKey := filepath.Join(filepath.Dir(legacy), "ssh_host_ed25519")
	canonicalKey := filepath.Join(filepath.Dir(canonical), "ssh_host_ed25519")
	if _, err := os.Stat(legacyKey); err == nil {
		if err := os.Rename(legacyKey, canonicalKey); err != nil {
			slog.Warn("conf: agent.json migrated but ssh_host_ed25519 rename failed; clients may see known_hosts mismatch",
				"from", legacyKey, "to", canonicalKey, "err", err)
		}
	}
	return nil
}

func suffixBak() string {
	return ".bak." + time.Now().UTC().Format("20060102-150405")
}
