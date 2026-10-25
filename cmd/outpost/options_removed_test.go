package main

import (
	"io"
	"strings"
	"testing"
)

// The option audit removed every flag that changes HOW outpost runs —
// update-mode/auto-upgrade style flags, legacy supervision switches,
// the auto-rollback arming flag. Only sharing/access/security options
// remain. These tests pin the removal: the old spellings are hard
// errors (unknown flag), not deprecated aliases, so scripts fail
// loudly instead of silently running a different lifecycle.
func TestRemovedRunModeFlagsAreGone(t *testing.T) {
	for _, flag := range []string{"--update", "--auto-upgrade", "--auto-rollback"} {
		cmd := builtinsSetCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{flag + "=auto"})
		if err := cmd.Execute(); err == nil {
			t.Errorf("builtins set %s: expected unknown-flag error, got nil", flag)
		} else if !strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("builtins set %s: expected unknown-flag error, got %v", flag, err)
		}
	}
}

// TestRemovedSupervisionSwitchesAreGone pins the pairing cleanup:
// `register` always installs the per-user service now — --no-service
// and the deprecated -y are gone.
func TestRemovedSupervisionSwitchesAreGone(t *testing.T) {
	for _, args := range [][]string{{"--no-service"}, {"-y"}} {
		cmd := registerCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(append(args, "--code", "x", "--server", "https://example.com", "--name", "h"))
		if err := cmd.Execute(); err == nil {
			t.Errorf("register %v: expected unknown-flag error, got nil", args)
		} else if !strings.Contains(err.Error(), "unknown flag") && !strings.Contains(err.Error(), "unknown shorthand") {
			t.Errorf("register %v: expected unknown-flag error, got %v", args, err)
		}
	}
}
