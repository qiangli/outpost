package main

import (
	"testing"

	"github.com/spf13/cobra"
)

// The outpost tree is fronted by bashy as `bashy outpost`, which owns
// completion on its side — so the root must not carry cobra's default
// `completion` subcommand anywhere in the tree.
func TestNoCompletionCommand(t *testing.T) {
	root := newRootCmd("test")
	if !root.CompletionOptions.DisableDefaultCmd {
		t.Error("root CompletionOptions.DisableDefaultCmd = false, want true")
	}
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, sub := range cmd.Commands() {
			if sub.Name() == "completion" {
				t.Errorf("found `completion` subcommand under %q", cmd.CommandPath())
			}
			walk(sub)
		}
	}
	walk(root)
}
