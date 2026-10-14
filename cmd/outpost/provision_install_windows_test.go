//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The rename-then-replace install path must hold on Windows, where a loaded
// image or DLL cannot be overwritten in place: replacing a live destination
// lands the new bytes with no backup or temp files left behind.
func TestInstallFile_ReplacesLiveDestinationWindows(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "prima-server-windows-amd64.exe")
	if err := os.WriteFile(dst, []byte("stuck old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installFile(dst, []byte("new verified bytes"), 0o755); err != nil {
		t.Fatalf("installFile over a live destination: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "new verified bytes" {
		t.Fatalf("dst holds %q, %v", data, err)
	}
	if _, err := os.Stat(dst + ".stale"); !os.IsNotExist(err) {
		t.Fatalf("aside backup left behind: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("dir holds %d entries, want only the replaced binary", len(entries))
	}
}
