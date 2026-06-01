// `outpost ssh ...` — the self-sufficient SSH subcommand tree.
//
// Wave 1 surface (this file): `list`, `add`, `rm`, `show`, `exec`.
// All five route through the local daemon's /mcp/ endpoint and call
// the matching `outpost_*_ssh_target` / `outpost_ssh_exec` tool,
// mirroring how `outpost apps` / `outpost outbound` work today.
//
// Wave 2 will add `connect` (interactive shell), `sftp`, `tunnel`,
// and the `outpost ssh <name>` shorthand for connect.
//
// Backwards compat: this is additive — existing `ssh-proxy` and
// `ssh-config` commands stay. Operators with hardcoded ~/.ssh/config
// stanzas keep working; new operators get the in-process path.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func sshTreeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ssh",
		Short: "Manage SSH targets and run remote commands without /usr/bin/ssh",
		Long: `outpost ssh ...

Friendly aliases for paired hosts plus an in-process SSH client that
reaches them through the matrix tunnel — no system /usr/bin/ssh
required, no ~/.ssh/config wiring needed for agentic callers.

Wave 1 surface:
  outpost ssh add <name> --host <paired-host> --user <os-user>
  outpost ssh list [--json]
  outpost ssh show <name>
  outpost ssh rm <name>
  outpost ssh exec <name> -- <command> [args...]

Once a target is added and you've run 'outpost connect <host>' to seed
an elevation cookie, 'outpost ssh exec <name> -- <cmd>' runs <cmd> on
the remote host and returns stdout/stderr + exit code — suitable for
both shell scripts and agentic-tool drive loops.`,
	}
	cmd.AddCommand(
		sshListCmd(),
		sshAddCmd(),
		sshShowCmd(),
		sshRmCmd(),
		sshExecCmd(),
	)
	return cmd
}

// listSSHTargetsViaMCP / sshTargetRow are extracted as small helpers
// so the JSON branch and the table branch don't drift.
type sshTargetRow struct {
	Name        string `json:"name"`
	Host        string `json:"host"`
	User        string `json:"user,omitempty"`
	Description string `json:"description,omitempty"`
}

func sshListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured SSH target aliases",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSSHList(cmd.Context(), jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	return cmd
}

func runSSHList(ctx context.Context, jsonOut bool) error {
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	var out struct {
		Targets []sshTargetRow `json:"targets"`
	}
	if err := session.callTool(ctx, "outpost_list_ssh_targets", map[string]any{}, &out); err != nil {
		return err
	}
	sort.Slice(out.Targets, func(i, j int) bool { return out.Targets[i].Name < out.Targets[j].Name })
	if jsonOut {
		b, _ := json.MarshalIndent(out.Targets, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	if len(out.Targets) == 0 {
		fmt.Println("No SSH targets configured. Use `outpost ssh add <name> --host <paired-host>` to add one.")
		return nil
	}
	fmt.Printf("%-20s  %-20s  %-12s  %s\n", "NAME", "HOST", "USER", "DESCRIPTION")
	for _, t := range out.Targets {
		fmt.Printf("%-20s  %-20s  %-12s  %s\n", t.Name, t.Host, t.User, t.Description)
	}
	return nil
}

func sshAddCmd() *cobra.Command {
	var (
		hostFlag string
		userFlag string
		descFlag string
	)
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Register a friendly alias for a paired host",
		Long: `Stores the alias under $XDG_CONFIG_HOME/outpost/ssh/<name>.json
(mode 0600). After adding, 'outpost ssh exec <name> -- <cmd>' runs
<cmd> on the configured host without further configuration.

--user defaults to the OS user the remote outpost runs as (reported
by cloudbox's /api/v1/ssh/hosts); pass --user only to override.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSSHAdd(cmd.Context(), args[0], hostFlag, userFlag, descFlag)
		},
	}
	cmd.Flags().StringVar(&hostFlag, "host", "", "Paired host name in cloudbox (e.g. novicortex). Required.")
	cmd.Flags().StringVar(&userFlag, "user", "", "OS user on the remote host (default: reported by /api/v1/ssh/hosts at exec time)")
	cmd.Flags().StringVar(&descFlag, "description", "", "Freeform note for `outpost ssh list`")
	return cmd
}

func runSSHAdd(ctx context.Context, name, host, user, description string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("--host is required (the paired-host name cloudbox routes to)")
	}
	// If --user is empty, try to resolve from cloudbox's /api/v1/ssh/hosts.
	// We do this here (in the CLI, not the daemon) so the on-disk
	// record carries everything ExecSSH needs without per-call lookups.
	if strings.TrimSpace(user) == "" {
		if u, ok, _ := resolveRemoteOSUser(ctx, host); ok {
			user = u
		}
	}
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	var out struct {
		Target sshTargetRow `json:"target"`
	}
	if err := session.callTool(ctx, "outpost_add_ssh_target", map[string]any{
		"name":        name,
		"host":        host,
		"user":        user,
		"description": description,
	}, &out); err != nil {
		return err
	}
	if out.Target.User == "" {
		fmt.Fprintf(os.Stderr, "warning: target %q has no user set — `outpost ssh exec` will refuse until you re-run `outpost ssh add %s --host %s --user <os-user>`.\n",
			out.Target.Name, out.Target.Name, out.Target.Host)
	}
	fmt.Printf("Added SSH target %q → %s (user=%s)\n", out.Target.Name, out.Target.Host, out.Target.User)
	return nil
}

// resolveRemoteOSUser asks cloudbox who the remote outpost runs as.
// Returns ("", false, nil) if cloudbox can't be reached or the host
// isn't in the list — the add proceeds with an empty user, and
// `outpost ssh add` warns the operator.
func resolveRemoteOSUser(ctx context.Context, host string) (string, bool, error) {
	cfgPath, err := conf.DefaultConfigPath()
	if err != nil {
		return "", false, err
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil || fc == nil {
		return "", false, err
	}
	bearer := strings.TrimSpace(os.Getenv("OUTPOST_SESSION_JWT"))
	if bearer == "" {
		bearer = fc.AccessToken
	}
	if bearer == "" {
		bearer = fc.Token
	}
	if bearer == "" || fc.ServerAddr == "" {
		return "", false, nil
	}
	hosts, err := fetchSSHHosts(ctx, fc.ServerAddr, fc.ServerPort, fc.Protocol, bearer)
	if err != nil {
		return "", false, nil
	}
	for _, h := range hosts {
		if strings.EqualFold(h.Host, host) && h.OsUser != "" {
			return h.OsUser, true, nil
		}
	}
	return "", false, nil
}

func sshShowCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Print one SSH target's details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				Targets []sshTargetRow `json:"targets"`
			}
			if err := session.callTool(cmd.Context(), "outpost_list_ssh_targets", map[string]any{}, &out); err != nil {
				return err
			}
			for _, t := range out.Targets {
				if t.Name == args[0] {
					if jsonOut {
						b, _ := json.MarshalIndent(t, "", "  ")
						fmt.Println(string(b))
						return nil
					}
					fmt.Printf("name:        %s\n", t.Name)
					fmt.Printf("host:        %s\n", t.Host)
					fmt.Printf("user:        %s\n", t.User)
					fmt.Printf("description: %s\n", t.Description)
					return nil
				}
			}
			return fmt.Errorf("no SSH target named %q", args[0])
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a record")
	return cmd
}

func sshRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Delete a configured SSH target",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				OK bool `json:"ok"`
			}
			if err := session.callTool(cmd.Context(), "outpost_remove_ssh_target", map[string]any{
				"name": args[0],
			}, &out); err != nil {
				return err
			}
			fmt.Printf("Removed SSH target %q\n", args[0])
			return nil
		},
	}
}

func sshExecCmd() *cobra.Command {
	var (
		timeoutFlag time.Duration
		jsonOut     bool
	)
	cmd := &cobra.Command{
		Use:   "exec <name> -- <command> [args...]",
		Short: "Run a one-shot command on a remote host through the matrix tunnel",
		Long: `Executes <command> on the named SSH target's remote host via the
in-process SSH client. Stdout goes to stdout, stderr to stderr, and
the CLI's exit code matches the remote process's exit code (0 on
success, the remote's non-zero code otherwise; 1 for an outpost-side
failure).

Requires a current elevation for the target's host — run
'outpost connect <host>' first (or 'outpost connect <host> --keep-alive'
to keep the cookie warm). If no elevation is present, exec returns
401 with guidance.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			// Cobra collapses "--" out of args by default; everything
			// after the alias is the command line.
			cmdLine := strings.Join(args[1:], " ")
			return runSSHExec(cmd.Context(), name, cmdLine, timeoutFlag, jsonOut)
		},
	}
	cmd.Flags().DurationVar(&timeoutFlag, "timeout", 60*time.Second,
		"Cap on remote runtime; capped server-side at 10m")
	cmd.Flags().BoolVar(&jsonOut, "json", false,
		"Emit the full result envelope as JSON instead of writing stdout/stderr through")
	return cmd
}

type sshExecResult struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
}

func runSSHExec(ctx context.Context, name, command string, timeout time.Duration, jsonOut bool) error {
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	var res sshExecResult
	if err := session.callTool(ctx, "outpost_ssh_exec", map[string]any{
		"name":            name,
		"command":         command,
		"timeout_seconds": int(timeout.Seconds()),
	}, &res); err != nil {
		return err
	}
	if jsonOut {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
	} else {
		if res.Stdout != "" {
			fmt.Print(res.Stdout)
		}
		if res.Stderr != "" {
			fmt.Fprint(os.Stderr, res.Stderr)
		}
		if res.StdoutTruncated {
			fmt.Fprintln(os.Stderr, "outpost ssh exec: stdout truncated (output exceeded the 1 MiB cap)")
		}
		if res.StderrTruncated {
			fmt.Fprintln(os.Stderr, "outpost ssh exec: stderr truncated (output exceeded the 256 KiB cap)")
		}
	}
	// Propagate the remote exit code so callers (shell scripts, agentic
	// loops) can branch on success/failure without parsing output.
	if res.ExitCode != 0 {
		os.Exit(res.ExitCode)
	}
	return nil
}
