package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/shell"
)

// outpost shell runs the local Bashy executable with the caller's stdio.
func shellCmd() *cobra.Command {
	var command string
	cmd := &cobra.Command{
		Use:   "shell",
		Short: "Run the paired Bashy shell on the local terminal",
		Long: `outpost shell launches Bashy, the same shell used by browser and SSH sessions.
It uses the local paired executable; missing userland is an explicit error.
Use -c "command" for one-shot execution with the command's exit status.`,
		Example: `  outpost shell
  outpost shell -c "echo hello"`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var (
				code int
				err  error
			)
			if cmd.Flags().Changed("command") {
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
