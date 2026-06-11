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
	"github.com/qiangli/outpost/internal/agent/discovery"
)

// isSavedSSHAlias reports whether `name` matches an existing target
// file on disk. Used by the cobra fall-through to decide whether a
// single positional arg is an alias (route to runSSHConnect) or an
// ad-hoc host (route to runSSHHost). Conservative: any error reading
// the path is treated as "not a saved alias" so the ad-hoc path
// stays usable when the targets dir is unreadable.
func isSavedSSHAlias(name string) bool {
	path, err := conf.SSHTargetPath(strings.TrimSpace(name))
	if err != nil {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return true
}

// lookupDiscoveredPeer scans the LAN for peers and returns the first
// one matching `name` (against AgentName or AssignedHostname). Used by
// `outpost ssh add --from-peer`.
func lookupDiscoveredPeer(ctx context.Context, name string) (*discovery.Peer, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	peers, err := discovery.Browse(ctx, discovery.BrowseOptions{Timeout: 3 * time.Second})
	if err != nil && len(peers) == 0 {
		return nil, fmt.Errorf("mdns browse: %w", err)
	}
	for _, p := range peers {
		if strings.EqualFold(p.AgentName, name) || strings.EqualFold(p.AssignedHostname, name) {
			return &p, nil
		}
	}
	return nil, fmt.Errorf("no LAN-discovered peer matching %q (run `outpost scan` to see what's visible)", name)
}

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
  outpost ssh [user@]<host> [cmd args...]  # ad-hoc; LAN-direct if reachable

Ad-hoc shorthand:
  'outpost ssh foo' or 'outpost ssh user@foo cmd args...' connects
  without a pre-saved alias. The command probes mDNS first — when
  the peer outpost is on the same LAN, it trades the cached cookie
  at cloudbox for a short-lived peer ticket and dials LAN-direct;
  otherwise falls back to the cloudbox-tunneled path. Passwordless
  after the first 'outpost connect'.

Hop/jump:
  Attach --via <alias> at add time (or --jump <alias> per-call) to
  ProxyJump through another configured target. Chains are walked
  outer-first (cloudbox → outer → ... → inner). The innermost target
  is the one your command runs on.`,
		// Allow bare 'outpost ssh [user@]<host> [cmd...]' to fall through.
		Args: cobra.ArbitraryArgs,
		// Runtime failures (host offline, handshake rejected, EAUTH)
		// are not usage errors — don't bury the actual message under
		// a usage dump.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			// Cobra dispatches known sub-verbs (list/add/...) first, so
			// anything reaching here is the ad-hoc shorthand: first arg
			// is [user@]host, remaining args are the remote command.
			//
			// Preserve backwards compat: if the first arg matches a
			// saved alias and no extra args follow, route to the
			// alias-driven runSSHConnect (which honors --via chains).
			// Otherwise treat as a raw [user@]host and route to
			// runSSHHost.
			first := args[0]
			if len(args) == 1 && isSavedSSHAlias(first) {
				return runSSHConnect(cmd.Context(), first, "")
			}
			return runSSHHost(cmd.Context(), first, args[1:])
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
		hostFlag     string
		userFlag     string
		descFlag     string
		viaFlag      string
		portFlag     int
		directFlag   bool
		fromPeerFlag string
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

--port defaults to 22 and is meaningful for hop targets (--via set) or
LAN-direct targets (--direct set).

--direct makes the target a LAN-direct dial: cloudbox is bypassed and
we open a plain TCP connection to <host>:<port> (defaults to 22). The
remote outpost must have its SSHListenAddr bound to a LAN address.
TOFU on the SSH host-key fingerprint applies (Wave 3A.2 lifts to
cloudbox-CA-signed certs).

--from-peer <peer-name> auto-fills --host / --port from a LAN-
discovered peer (matched by AgentName or AssignedHostname; same
data 'outpost scan' shows). The first lan-ssh endpoint wins; override
with explicit --host/--port.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSSHAdd(cmd.Context(), args[0], hostFlag, userFlag, descFlag, viaFlag, portFlag, directFlag, fromPeerFlag)
		},
	}
	cmd.Flags().StringVar(&hostFlag, "host", "", "Destination: paired host name (default), hop-side address (with --via), or LAN IP/hostname (with --direct). Required unless --from-peer is set.")
	cmd.Flags().StringVar(&userFlag, "user", "", "OS user on the remote host (default: reported by /api/v1/ssh/hosts at exec time)")
	cmd.Flags().StringVar(&descFlag, "description", "", "Freeform note for `outpost ssh list`")
	cmd.Flags().StringVar(&viaFlag, "via", "", "ProxyJump-style hop: alias of another configured target to dial through first")
	cmd.Flags().IntVar(&portFlag, "port", 0, "SSH port (default 22; ignored unless --via or --direct is set)")
	cmd.Flags().BoolVar(&directFlag, "direct", false, "LAN-direct dial: bypass cloudbox and connect to <host>:<port> over plain TCP")
	cmd.Flags().StringVar(&fromPeerFlag, "from-peer", "", "Auto-fill from a mDNS-discovered peer (uses the first lan-ssh endpoint). Implies --direct.")
	return cmd
}

func runSSHAdd(ctx context.Context, name, host, user, description, via string, port int, direct bool, fromPeer string) error {
	// --from-peer: discover the peer on LAN, pull its endpoint + user
	// + assigned hostname into the target.
	if fromPeer = strings.TrimSpace(fromPeer); fromPeer != "" {
		direct = true
		discovered, err := lookupDiscoveredPeer(ctx, fromPeer)
		if err != nil {
			return err
		}
		ep := discovered.FirstEndpoint(discovery.EndpointLANSSH)
		if ep.Host == "" || ep.Port == 0 {
			return fmt.Errorf("peer %q has no lan-ssh endpoint advertised; add SSHListenAddr on that outpost", fromPeer)
		}
		if host == "" {
			host = ep.Host
		}
		if port == 0 {
			port = ep.Port
		}
		if user == "" {
			user = discovered.OSUsername
		}
	}

	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("--host is required (the destination's address)")
	}
	// --user resolution from cloudbox makes sense only when reaching
	// a paired outpost directly through cloudbox. Hop and LAN-direct
	// targets reach raw hosts; the operator must supply --user.
	if strings.TrimSpace(user) == "" && strings.TrimSpace(via) == "" && !direct {
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
	if direct {
		args["direct"] = true
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

For cloudbox-tunneled targets, requires a current elevation for the
target's host — run 'outpost connect <host>' first (or
'outpost connect <host> --keep-alive' to keep the cookie warm).
If no elevation is present, exec returns 401 with guidance.

LAN-direct targets reuse the cached peer ticket and do NOT require
re-elevation — once seeded with the first 'outpost connect', exec works
without further interaction until the peer ticket expires.

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
