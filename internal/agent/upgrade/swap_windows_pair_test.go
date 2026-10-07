//go:build windows

package upgrade

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsImmediatePairRestoreUsesDistinctParkedImages(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "outpost.exe")
	candidate := filepath.Join(dir, "next.exe")
	restore := filepath.Join(dir, "restore.exe")
	for path, body := range map[string]string{current: "old", candidate: "new", restore: "old"} {
		if err := os.WriteFile(path, []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := SwapAtomic(current, candidate); err != nil {
		t.Fatal(err)
	}
	if err := SwapAtomic(current, restore); err != nil {
		t.Fatalf("immediate rollback: %v", err)
	}
	parked, err := filepath.Glob(current + ".replaced-*")
	if err != nil || len(parked) != 2 {
		t.Fatalf("each swap needs a unique parked image: %v %v", parked, err)
	}
	data, err := os.ReadFile(current)
	if err != nil || string(data) != "old" {
		t.Fatalf("restore = %q %v", data, err)
	}
}
