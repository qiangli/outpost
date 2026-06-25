// `outpost jobs / fg / bg / kill` are the external job-control commands the
// matrix shell points users at when they try fg/bg/jobs in-shell — those
// builtins can't own the controlling terminal (subshells in qiangli/sh are
// goroutines, not real OS processes). Outpost records each detached PID via the
// WithBgPidCallback hook on the shell runner (see internal/agent/shell); these
// commands read that persistent registry.
//
// The job-control logic — the PID registry plus signal-based fg(=wait)/bg/kill
// and the Windows "not supported" stubs — is the shared coreutils/pkg/jobs
// package (also used by `bashy jobs/fg/bg/kill`). These thin wrappers just
// expose its cobra commands under the outpost root, so there is no duplication.
package main

import (
	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/jobs"
)

// Preserve the on-disk jobs directory (<UserCacheDir>/outpost/jobs) that hosts
// already use, rather than the package default ("bashy").
func init() { jobs.AppName = "outpost" }

func jobsCmd() *cobra.Command { return jobs.JobsCommand() }
func fgCmd() *cobra.Command   { return jobs.FgCommand() }
func bgCmd() *cobra.Command   { return jobs.BgCommand() }
func killCmd() *cobra.Command { return jobs.KillCommand() }
