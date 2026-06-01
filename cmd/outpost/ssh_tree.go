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

Surface:
  outpost ssh add <name> --host <paired-host> --user <os-user> [--via <alias>]
  outpost ssh list [--json]
  outpost ssh show <name>
  outpost ssh rm <name>
  outpost ssh exec <name> [--jump <alias>] -- <command> [args...]
  outpost ssh connect <name> [--jump <alias>]
  outpost ssh tunnel <name> [--jump <alias>] -L <local>:<remote-host>:<remote-port>
  outpost ssh sftp <name> [--jump <alias>] (get|put|ls) ...
  outpost ssh <name>                       # shorthand for 'connect <name>'

Hop/jump:
  Attach --via <alias> at add time (or --jump <alias> per-call) to
  ProxyJump through another configured target. Chains are walked
  outer-first (cloudbox → outer → ... → inner). The innermost target
  is the one your command runs on.`,
		// Allow bare 'outpost ssh <name>' to fall through to connect.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			// If the first arg matches a known subcommand we never get
			// here (cobra dispatches). So a single arg here is treated
			// as a target alias for the shorthand-connect.
			if len(args) == 1 {
				return runSSHConnect(cmd.Context(), args[0], "")
			}
			return fmt.Errorf("unknown subcommand %q (try 'outpost ssh --help')", args[0])
		},
	}
	cmd.AddCommand(
		sshListCmd(),
		sshAddCmd(),
		sshShowCmd(),
		sshRmCmd(),
		sshExecCmd(),
		sshConnectCmd(),
		sshTunnelCmd(),
		sshSFTPCmd(),
	)
	return cmd
}

// listSSHTargetsViaMCP / sshTargetRow are extracted as small helpers
// so the JSON branch and the table branch don't drift.
type sshTargetRow struct {
	Name        string `json:"name"`
	Host        string `json:"host"`
	User        string `json:"user,omitempty"`
	Port        int    `json:"port,omitempty"`
	Via         string `json:"via,omitempty"`
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
	fmt.Printf("%-20s  %-20s  %-12s  %-16s  %s\n", "NAME", "HOST", "USER", "VIA", "DESCRIPTION")
	for _, t := range out.Targets {
		via := t.Via
		if via == "" {
			via = "-"
		}
		fmt.Printf("%-20s  %-20s  %-12s  %-16s  %s\n", t.Name, t.Host, t.User, via, t.Description)
	}
	return nil
}

func sshAddCmd() *cobra.Command {
	var (
		hostFlag string
		userFlag string
		descFlag string
		viaFlag  string
		portFlag int
	)
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Register a friendly alias for a paired host (with optional hop)",
		Long: `Stores the alias under $XDG_CONFIG_HOME/outpost/ssh/<name>.json
(mode 0600). After adding, 'outpost ssh exec <name> -- <cmd>' runs
<cmd> on the configured host without further configuration.

--user defaults to the OS user the remote outpost runs as (reported
by cloudbox's /api/v1/ssh/hosts at exec time); pass --user to override.

--via <alias> sets a ProxyJump-style hop: dialing this target first
dials <alias>, then opens a direct-tcpip channel to <host>:<port>
through it. Chains are walked recursively (max 8 hops). For paired
peer destinations the remote outpost's peerhosts allowlist accepts
the channel automatically; for other LAN destinations the operator
must have widened SSHAllowLocalForward.

--port defaults to 22 and is only meaningful when --via is set
(direct dials to cloudbox-paired hosts don't carry a port).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSSHAdd(cmd.Context(), args[0], hostFlag, userFlag, descFlag, viaFlag, portFlag)
		},
	}
	cmd.Flags().StringVar(&hostFlag, "host", "", "Destination: paired host name (when --via is empty), or hop-side address (when --via is set). Required.")
	cmd.Flags().StringVar(&userFlag, "user", "", "OS user on the remote host (default: reported by /api/v1/ssh/hosts at exec time)")
	cmd.Flags().StringVar(&descFlag, "description", "", "Freeform note for `outpost ssh list`")
	cmd.Flags().StringVar(&viaFlag, "via", "", "ProxyJump-style hop: alias of another configured target to dial through first")
	cmd.Flags().IntVar(&portFlag, "port", 0, "SSH port on the hop destination (default 22; ignored unless --via is set)")
	return cmd
}

func runSSHAdd(ctx context.Context, name, host, user, description, via string, port int) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("--host is required (the destination's address)")
	}
	// --user resolution from cloudbox makes sense only when reaching
	// a paired outpost directly. Hop targets reach raw hosts; the
	// operator must supply --user themselves.
	if strings.TrimSpace(user) == "" && strings.TrimSpace(via) == "" {
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
	args := map[string]any{
		"name":        name,
		"host":        host,
		"user":        user,
		"description": description,
	}
	if via != "" {
		args["via"] = via
	}
	if port > 0 {
		args["port"] = port
	}
	if err := session.callTool(ctx, "outpost_add_ssh_target", args, &out); err != nil {
		return err
	}
	if out.Target.User == "" {
		fmt.Fprintf(os.Stderr, "warning: target %q has no user set — `outpost ssh exec` will refuse until you re-run `outpost ssh add %s --host %s --user <os-user>`.\n",
			out.Target.Name, out.Target.Name, out.Target.Host)
	}
	suffix := ""
	if out.Target.Via != "" {
		suffix = fmt.Sprintf(" via %s", out.Target.Via)
	}
	fmt.Printf("Added SSH target %q → %s (user=%s)%s\n", out.Target.Name, out.Target.Host, out.Target.User, suffix)
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
					if t.Via != "" {
						fmt.Printf("via:         %s\n", t.Via)
					}
					if t.Port > 0 {
						fmt.Printf("port:        %d\n", t.Port)
					}
					if t.Description != "" {
						fmt.Printf("description: %s\n", t.Description)
					}
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
		jumpFlag    string
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
401 with guidance.

--jump <alias> overrides the target's persisted Via for this one
call (analogous to ssh's -J).`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cmdLine := strings.Join(args[1:], " ")
			return runSSHExec(cmd.Context(), name, cmdLine, jumpFlag, timeoutFlag, jsonOut)
		},
	}
	cmd.Flags().DurationVar(&timeoutFlag, "timeout", 60*time.Second,
		"Cap on remote runtime; capped server-side at 10m")
	cmd.Flags().BoolVar(&jsonOut, "json", false,
		"Emit the full result envelope as JSON instead of writing stdout/stderr through")
	cmd.Flags().StringVar(&jumpFlag, "jump", "",
		"ProxyJump alias for this call (overrides the target's persisted --via)")
	return cmd
}

type sshExecResult struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
}

func runSSHExec(ctx context.Context, name, command, jump string, timeout time.Duration, jsonOut bool) error {
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	var res sshExecResult
	args := map[string]any{
		"name":            name,
		"command":         command,
		"timeout_seconds": int(timeout.Seconds()),
	}
	if jump != "" {
		args["jump"] = jump
	}
	if err := session.callTool(ctx, "outpost_ssh_exec", args, &res); err != nil {
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
