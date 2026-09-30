//go:build !windows

package agent

import (
	"os"
	"syscall"
)

func authorizedKeysOwnedByCurrentUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
