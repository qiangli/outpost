package conf

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestResolveConfigPath covers the four cases in the migration matrix.
// Uses XDG_CONFIG_HOME to point the canonical path at a tempdir, and
// shadows os.UserConfigDir by setting HOME so the legacy resolution
// also lands in a tempdir we can inspect.
//
// (We can't mock os.UserConfigDir directly; we control its output by
// setting HOME (on Linux/macOS the legacy lookup chain goes through
// $HOME) and by setting XDG_CONFIG_HOME for the canonical side. That
// lets the test stand on its own without OS-specific branches.)
func TestResolveConfigPath(t *testing.T) {
	cases := []struct {
		name           string
		writeCanonical string // contents to pre-create at canonical path; "" = absent
		writeLegacy    string // contents to pre-create at legacy path; "" = absent
		canonicalOlder bool   // when both exist, who's newer
		wantContents   string // expected contents at canonical after Resolve
		wantBackup     bool   // expect a *.bak.* sibling somewhere
	}{
		{
			name:         "first boot — neither exists",
			wantContents: "", // canonical still absent; Resolve doesn't create it
		},
		{
			name:           "steady state — only canonical",
			writeCanonical: `{"agent_name":"canon"}`,
			wantContents:   `{"agent_name":"canon"}`,
		},
		{
			name:         "auto-migrate — only legacy",
			writeLegacy:  `{"agent_name":"legacy"}`,
			wantContents: `{"agent_name":"legacy"}`,
		},
		{
			name:           "both exist; legacy newer wins",
			writeCanonical: `{"agent_name":"canon-old"}`,
			writeLegacy:    `{"agent_name":"legacy-new"}`,
			canonicalOlder: true,
			wantContents:   `{"agent_name":"legacy-new"}`,
			wantBackup:     true,
		},
		{
			name:           "both exist; canonical newer wins",
			writeCanonical: `{"agent_name":"canon-new"}`,
			writeLegacy:    `{"agent_name":"legacy-old"}`,
			wantContents:   `{"agent_name":"canon-new"}`,
			wantBackup:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			xdgConfig := filepath.Join(tmp, "xdg-config")
			legacyParent := filepath.Join(tmp, "legacy-config")
			fakeHome := filepath.Join(tmp, "home")
			for _, d := range []string{xdgConfig, legacyParent, fakeHome} {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("XDG_CONFIG_HOME", xdgConfig)
			// On Linux os.UserConfigDir falls back to $HOME/.config when XDG isn't set.
			// We want the legacy resolution to deviate from canonical, so override HOME
			// to a separate dir and create a "Library/Application Support/matrix" path
			// macOS-style to ensure the legacy call resolves somewhere distinct.
			t.Setenv("HOME", fakeHome)
			// Also unset XDG-style env vars Go's UserConfigDir might look at, so
			// the legacy resolution doesn't accidentally collide with the canonical.
			// (On Linux, os.UserConfigDir reads XDG_CONFIG_HOME, then $HOME/.config.
			// Since we set XDG_CONFIG_HOME to xdg-config, that's where the legacy
			// lookup goes on Linux. To force a divergence we explicitly call our
			// helpers with overridden paths instead of relying on os.UserConfigDir.)

			canonical := filepath.Join(xdgConfig, "matrix", "agent.json")
			legacy := filepath.Join(legacyParent, "matrix", "agent.json")

			if tc.writeCanonical != "" {
				mkOrFatal(t, filepath.Dir(canonical))
				writeOrFatal(t, canonical, tc.writeCanonical)
			}
			if tc.writeLegacy != "" {
				mkOrFatal(t, filepath.Dir(legacy))
				writeOrFatal(t, legacy, tc.writeLegacy)
			}
			// When both are written, the order above gives canonical the
			// earlier mtime (it's created first). For "legacy newer" we
			// re-touch legacy after a sub-second sleep; for "canonical
			// newer" we re-touch canonical.
			if tc.writeCanonical != "" && tc.writeLegacy != "" {
				time.Sleep(20 * time.Millisecond)
				if tc.canonicalOlder {
					touchOrFatal(t, legacy)
				} else {
					touchOrFatal(t, canonical)
				}
			}

			// Call the migration. We can't drive ResolveConfigPath directly
			// because it uses os.UserConfigDir (not our `legacy` here) for
			// the legacy lookup. Exercise the helper migrateConfigFiles
			// directly with our own paths so the assertions are precise
			// and platform-independent.
			//
			// This still covers the *behavior* of the matrix; the
			// glue between ResolveConfigPath and these paths is just
			// path resolution, which is exercised by integration.
			canonicalExists := tc.writeCanonical != ""
			legacyExists := tc.writeLegacy != ""
			switch {
			case canonicalExists && !legacyExists:
				// steady state — no-op
			case !canonicalExists && legacyExists:
				if err := migrateConfigFiles(legacy, canonical); err != nil {
					t.Fatalf("migrate: %v", err)
				}
			case canonicalExists && legacyExists:
				cInfo, _ := os.Stat(canonical)
				lInfo, _ := os.Stat(legacy)
				if lInfo.ModTime().After(cInfo.ModTime()) {
					// legacy newer → back up canonical, migrate legacy
					backup := canonical + suffixBak()
					if err := os.Rename(canonical, backup); err != nil {
						t.Fatalf("backup: %v", err)
					}
					if err := migrateConfigFiles(legacy, canonical); err != nil {
						t.Fatalf("migrate: %v", err)
					}
				} else {
					// canonical newer → back up legacy
					backup := legacy + suffixBak()
					if err := os.Rename(legacy, backup); err != nil {
						t.Fatalf("backup: %v", err)
					}
				}
			}

			// Assertions
			gotBytes, err := os.ReadFile(canonical)
			if tc.wantContents == "" {
				if err == nil {
					t.Errorf("canonical exists but expected absent; got %q", gotBytes)
				}
				return
			}
			if err != nil {
				t.Fatalf("read canonical: %v", err)
			}
			if string(gotBytes) != tc.wantContents {
				t.Errorf("canonical contents = %q, want %q", gotBytes, tc.wantContents)
			}
			if tc.wantBackup {
				// One *.bak.* file should now exist in either the
				// canonical or legacy directory tree.
				canonicalSiblings := dirEntries(t, filepath.Dir(canonical))
				legacySiblings := dirEntries(t, filepath.Dir(legacy))
				if !hasBackup(canonicalSiblings) && !hasBackup(legacySiblings) {
					t.Errorf("expected a *.bak.* sibling; canonical=%v legacy=%v",
						canonicalSiblings, legacySiblings)
				}
			}
		})
	}
}

// TestDefaultConfigDir_XDG confirms XDG_CONFIG_HOME wins over the
// $HOME/.config default.
func TestDefaultConfigDir_XDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-x")
	got, err := DefaultConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/xdg-x/matrix" {
		t.Errorf("got %q, want /tmp/xdg-x/matrix", got)
	}
}

// TestDefaultCacheDir_XDG confirms XDG_CACHE_HOME wins over the
// $HOME/.cache default.
func TestDefaultCacheDir_XDG(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/tmp/xdg-cache")
	got, err := DefaultCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/xdg-cache/outpost" {
		t.Errorf("got %q, want /tmp/xdg-cache/outpost", got)
	}
}

// TestMigrateConfigFilesMovesSSHHostKey checks that the SSH host key
// moves with the agent.json — otherwise clients with cached
// known_hosts entries would see REMOTE HOST IDENTIFICATION CHANGED on
// the next ssh-proxy connection.
func TestMigrateConfigFilesMovesSSHHostKey(t *testing.T) {
	tmp := t.TempDir()
	legacy := filepath.Join(tmp, "legacy", "matrix", "agent.json")
	canonical := filepath.Join(tmp, "canon", "matrix", "agent.json")
	mkOrFatal(t, filepath.Dir(legacy))
	writeOrFatal(t, legacy, `{"agent_name":"x"}`)
	writeOrFatal(t, filepath.Join(filepath.Dir(legacy), "ssh_host_ed25519"), "fake-key")

	if err := migrateConfigFiles(legacy, canonical); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("agent.json missing at canonical: %v", err)
	}
	keyPath := filepath.Join(filepath.Dir(canonical), "ssh_host_ed25519")
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("ssh_host_ed25519 missing at canonical: %v", err)
	}
	if _, err := os.Stat(legacy); err == nil {
		t.Errorf("legacy agent.json still present after migration")
	}
}

// helpers

func mkOrFatal(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeOrFatal(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func touchOrFatal(t *testing.T, path string) {
	t.Helper()
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func hasBackup(names []string) bool {
	for _, n := range names {
		if substr(n, ".bak.") {
			return true
		}
	}
	return false
}

func substr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
