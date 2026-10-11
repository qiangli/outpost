//go:build !windows

package vknode

import (
	"os"
	"testing"
)

func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %o, want 0600", info.Mode().Perm())
	}
}
