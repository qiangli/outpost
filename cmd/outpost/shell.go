package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/shell"
)

// outpost shell — drive the in-process bash interpreter
// (qiangli/sh fork) against the caller's local terminal. Same
// interpreter, builtins (disown/jobs/fg/bg/kill/nohup/setsid),
// history file, and detached-job registry the matrix-shell and SSH
// paths use; no WebSocket, no SSH, no internal PTY allocation — the
// caller's terminal is the TTY.
//
// Two modes:
//   - bare `outpost shell`  → interactive read-edit-execute loop
//   - `outpost shell -c CMD` → one-shot, returns CMD's exit status
//
// Exit code from `exit N` (or from -c CMD) propagates as the process
// exit code, so shell pipelines treat outpost shell like any other
// shell.
func shellCmd() *cobra.Command {
	var command string
	cmd := &cobra.Command{
		Use:   "shell",
		Short: "Local interactive shell (same in-process bash the matrix-shell uses)",
		Long: `outpost shell launches the same in-process bash interpreter that
backs the matrix shell (browser PTY) and the built-in SSH server,
but bound to your local terminal. No WebSocket, no SSH server, no
internal PTY allocation — your terminal IS the TTY.

Shared with the matrix-shell path:
  - bash builtins from the qiangli/sh fork: disown, jobs, fg, bg,
    kill, nohup, setsid
  - history file at $OUTPOST_SHELL_HISTORY (default
    <cache>/outpost/shell_history)
  - detached-job registry — a 'nohup foo &' here shows up under
    'outpost jobs' and survives the shell exit

Use -c "command" for one-shot execution, like bash -c.`,
		Example: `  outpost shell
  outpost shell -c "echo hello"
  outpost shell -c "nohup long-job &"     # registers via 'outpost jobs'`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var (
				code int
				err  error
			)
			if command != "" {
				code, err = shell.RunLocalCommand(cmd.Context(), command, nil, nil, nil)
			} else {
				code, err = shell.RunLocal(cmd.Context())
			}
			if err != nil {
				return err
			}
			if code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&command, "command", "c", "", "Run COMMAND string once and exit (like bash -c)")
	return cmd
}
