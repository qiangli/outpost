//go:build !windows

package backup

import (
	"os"
	"testing"
)

// assertSecretPerm locks in the POSIX half of the secret-file
// confidentiality contract: mode 0600, owner-only.
func assertSecretPerm(t *testing.T, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("%s mode = %#o, want 0600", path, mode)
	}
}
