//go:build !windows

package heartbeat

import (
	"bytes"
	"context"
	"os"
	"path/filepath"

	"testing"
)

// The filesystem has a synthetic capacity of 100 bytes. The files themselves
// are real. Run the same probe used inside WSL with fixture stat/du commands.
func TestDiskGuestWriteFixture(t *testing.T) {
	dir := t.TempDir()
	logs := filepath.Join(dir, "outpost")
	cache := filepath.Join(dir, "modules")
	commands := filepath.Join(dir, "bin")
	for _, p := range []string{logs, cache, commands} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string, data []byte, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	cached := filepath.Join(cache, "archive")
	log := filepath.Join(logs, "daemon.log")
	write(cached, bytes.Repeat([]byte("m"), 74), 0600)
	write(log, []byte("small"), 0600)
	write(filepath.Join(commands, "stat"), []byte(`#!/bin/sh
case "$3" in
 "%i") echo a ;;
 *) used=$(wc -c < "$GOMODCACHE/archive"); n=$(wc -c < "$XDG_CACHE_HOME/outpost/daemon.log")
 printf '%s a 1 100 %s\n' "${3%% *}" "$((100-used-n))" ;;
esac
`), 0700)
	write(filepath.Join(commands, "du"), []byte(`#!/bin/sh
for p do :; done
[ -d "$p" ] && p="$p/archive"
n=$(wc -c < "$p") || exit 1
printf '%d\tfixture\n' "$n"
`), 0700)
	write(filepath.Join(commands, "timeout"), []byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0700)
	t.Setenv("PATH", commands+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", dir)
	t.Setenv("GOCACHE", filepath.Join(dir, "missing"))
	t.Setenv("BASHY_OTEL_SPOOL", filepath.Join(dir, "missing"))
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("GOMODCACHE", cache)
	sample := func() measurement {
		t.Helper()
		rows := linuxMeasurements(context.Background(), "wsl", "fixture", "WSL Fixture", "sh", "-c", diskScript)
		if len(rows) != 1 {
			t.Fatalf("volumes %d", len(rows))
		}
		return rows[0]
	}
	before := sample()
	write(log, []byte("growing log"), 0600)
	after := sample()
	v := after.report(before)
	if before.Used != 79 || v.Used != 85 || v.DisplayName != "WSL Fixture /" || len(v.Growers) != 1 || v.Growers[0] != (grower{"logs", 6}) {
		t.Fatalf("crossing %d -> %+v", before.Used, v)
	}
	b, err := os.ReadFile(cached)
	if err != nil || !bytes.Equal(b, bytes.Repeat([]byte("m"), 74)) {
		t.Fatal("persistent module changed")
	}
}
