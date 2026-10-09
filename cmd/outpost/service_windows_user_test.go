//go:build windows

package main

import (
	"os/user"
	"testing"
)

// A workgroup ssh session can report USERDOMAIN=WORKGROUP; the task principal
// must still be the token's own account so Task Scheduler can resolve it.
func TestWindowsUserIDUsesTokenAccount(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Skipf("no token account: %v", err)
	}
	t.Setenv("USERDOMAIN", "WORKGROUP")
	if got := windowsUserID(); got != u.Username {
		t.Fatalf("windowsUserID() = %q, want token account %q", got, u.Username)
	}
}
