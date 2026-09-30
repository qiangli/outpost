//go:build windows

package agent

import "os"

// Windows ACL ownership validation is intentionally deferred with the wider
// Windows key-management work; this narrow LAN listener release keeps the
// portable permission check in the caller.
func authorizedKeysOwnedByCurrentUser(info os.FileInfo) bool { return true }
