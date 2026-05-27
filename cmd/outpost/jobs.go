//go:build !windows

// `outpost jobs / fg / bg / kill` are the external job-control commands
// the matrix shell points users at when they try to run `fg`/`bg`/`jobs`
// in-shell — those builtins can't work because subshells in the
// qiangli/sh interpreter are goroutines, not real OS processes. Outpost
// records each detached PID via the WithBgPidCallback hook on the shell
// runner; these commands read that persistent registry.
//
// Windows: see jobs_windows.go — the signal-based job-control model is
// Unix-specific (no SIGSTOP/SIGCONT/SIGUSR1/SIGUSR2 equivalents on
// Windows), so the verbs stub out to "not supported."
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/shell"
)

func jobsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "jobs",
		Short: "List detached background jobs spawned by the matrix shell on this host",
		Long: `List background jobs that the matrix shell (the in-process qiangli/sh
runner reached over /shell and /ssh) detached via "nohup foo &", "setsid
foo &", or plain "foo &" followed by client disconnect.

Records live at <UserCacheDir>/outpost/jobs/<pid>.json. Dead records
(PID no longer running) are pruned as a side effect of listing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rows, err := shell.DefaultRegistry().List()
			if err != nil {
				return fmt.Errorf("read registry: %w", err)
			}
			if len(rows) == 0 {
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "PID\tUSER\tSTARTED\tELAPSED\tCMD")
			now := time.Now().UTC()
			for _, r := range rows {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
					r.PID, r.User,
					r.StartedAt.Local().Format("2006-01-02 15:04:05"),
					formatElapsed(now.Sub(r.StartedAt)),
					r.Cmd,
				)
			}
			return tw.Flush()
		},
	}
}

func fgCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fg <pid>",
		Short: "Wait for a recorded background job to exit",
		Long: `Block until the named PID is no longer running. Polls with
syscall.Kill(pid, 0) every 250 ms — the OS exit status is not captured
(this is the qiangli/sh "detached process" trade-off), so fg always
returns 0 on a natural exit and 1 only if the PID can't be polled.

Stdio cannot be re-attached once a job has detached — that's the price
of "nohup foo &" surviving the client's disconnect. Use this command
when scripting "wait for that long-running job, then continue".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, err := strconv.Atoi(args[0])
			if err != nil || pid <= 0 {
				return fmt.Errorf("invalid pid %q", args[0])
			}
			if _, err := shell.DefaultRegistry().Get(pid); err != nil {
				return fmt.Errorf("no recorded job for pid %d", pid)
			}
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-ticker.C:
					if err := syscall.Kill(pid, 0); err != nil {
						if errors.Is(err, syscall.ESRCH) {
							_ = shell.DefaultRegistry().Delete(pid)
							return nil
						}
						return fmt.Errorf("poll pid %d: %w", pid, err)
					}
				}
			}
		},
	}
}

func bgCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bg <pid>",
		Short: "No-op (kept for symmetry with the in-shell `bg` users expect)",
		Long: `The matrix shell does not implement Ctrl-Z / SIGTSTP, so there is no
"suspended" state for "bg" to resume from. This command exists so the
fork's hint for the unimplemented in-shell ` + "`bg`" + ` builtin has a
landing surface; it prints a one-line note and exits 0.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(),
				"outpost bg: matrix shell has no SIGTSTP forwarding; jobs run in the background from the start (`foo &`). No-op.")
			return nil
		},
	}
}

func killCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kill <pid> [SIGNAL]",
		Short: "Send a signal to a recorded background job and forget it",
		Long: `Default SIGTERM; pass a signal name (HUP/INT/TERM/KILL/USR1/USR2/...)
or a number as the second arg. On success the registry entry is
deleted — a stale ` + "`outpost jobs`" + ` row for a killed process is the
common UX bug this avoids.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, err := strconv.Atoi(args[0])
			if err != nil || pid <= 0 {
				return fmt.Errorf("invalid pid %q", args[0])
			}
			sig := syscall.SIGTERM
			if len(args) == 2 {
				sig, err = parseSignal(args[1])
				if err != nil {
					return err
				}
			}
			if err := syscall.Kill(pid, sig); err != nil {
				if errors.Is(err, syscall.ESRCH) {
					// Process already gone — drop the registry row and
					// treat as success so scripts can be idempotent.
					_ = shell.DefaultRegistry().Delete(pid)
					return nil
				}
				return fmt.Errorf("kill pid %d: %w", pid, err)
			}
			if err := shell.DefaultRegistry().Delete(pid); err != nil {
				// Signal sent; failing to clean up the registry row isn't
				// fatal — the next `outpost jobs` will prune via pidAlive.
				fmt.Fprintf(os.Stderr, "warning: removed signal but registry cleanup failed: %v\n", err)
			}
			return nil
		},
	}
}

// parseSignal accepts "HUP" / "SIGHUP" / "1" / etc. Limited table:
// only the signals embedders commonly want for job control. Numeric
// values bypass the table.
func parseSignal(s string) (syscall.Signal, error) {
	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("invalid signal number %d", n)
		}
		return syscall.Signal(n), nil
	}
	name := strings.ToUpper(strings.TrimSpace(s))
	name = strings.TrimPrefix(name, "SIG")
	switch name {
	case "HUP":
		return syscall.SIGHUP, nil
	case "INT":
		return syscall.SIGINT, nil
	case "QUIT":
		return syscall.SIGQUIT, nil
	case "TERM":
		return syscall.SIGTERM, nil
	case "KILL":
		return syscall.SIGKILL, nil
	case "USR1":
		return syscall.SIGUSR1, nil
	case "USR2":
		return syscall.SIGUSR2, nil
	case "STOP":
		return syscall.SIGSTOP, nil
	case "CONT":
		return syscall.SIGCONT, nil
	}
	return 0, fmt.Errorf("unknown signal %q (try HUP, INT, TERM, KILL, USR1, USR2, STOP, CONT, or a number)", s)
}

func formatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
