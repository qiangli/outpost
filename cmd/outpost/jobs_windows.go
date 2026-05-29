//go:build windows

// Windows stubs for jobs / fg / bg / kill. The Unix implementation
// (jobs.go) relies on signal-based job control — SIGSTOP / SIGCONT /
// SIGUSR1 / SIGUSR2 / kill -0 for liveness — none of which translate
// to Windows. The verbs are still registered (so `outpost --help`
// stays consistent across platforms) but each one prints a "not
// supported on Windows" notice and exits non-zero.

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func jobsCmd() *cobra.Command { return notSupportedCmd("jobs") }
func fgCmd() *cobra.Command   { return notSupportedCmd("fg") }
func bgCmd() *cobra.Command   { return notSupportedCmd("bg") }
func killCmd() *cobra.Command { return notSupportedCmd("kill") }

func notSupportedCmd(name string) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: "(Unix-only) not supported on Windows",
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("`outpost %s` relies on signal-based job control and is not available on Windows", name)
		},
	}
}
