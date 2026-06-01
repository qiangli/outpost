// Interactive verbs in the `outpost ssh ...` tree: connect (shell),
// tunnel (-L), and sftp (file transfer). These dial cloudbox
// directly via ssh_dial.go's dialSSHTargetChain helper rather than
// routing through MCP — Shell + SFTP + persistent tunnel listeners
// don't fit the per-call MCP roundtrip shape.
//
// The MCP `outpost_ssh_exec` tool stays in tools_ssh.go for agentic
// callers that want structured one-shot exec; this file is the human-
// operator path.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pkg/sftp"
	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/sshclient"
)

func sshConnectCmd() *cobra.Command {
	var jumpFlag string
	cmd := &cobra.Command{
		Use:   "connect <name>",
		Short: "Open an interactive shell on a configured target",
		Long: `Opens an interactive shell on the named target's remote host
using outpost's in-process SSH client. The local terminal is put
into raw mode for the duration of the session; window-resize is
forwarded to the remote PTY.

--jump <alias> overrides the target's persisted Via for this one
call (analogous to ssh's -J).

Shortcut: 'outpost ssh <name>' is equivalent to 'outpost ssh connect <name>'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSSHConnect(cmd.Context(), args[0], jumpFlag)
		},
	}
	cmd.Flags().StringVar(&jumpFlag, "jump", "", "ProxyJump alias for this call (overrides the target's persisted --via)")
	return cmd
}

func runSSHConnect(ctx context.Context, name, jump string) error {
	client, cleanup, err := dialSSHTargetChain(ctx, name, jump)
	if err != nil {
		return err
	}
	defer cleanup()
	exit, err := client.Shell(ctx, sshclient.ShellOptions{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		// TermType, Width, Height defaults pick up the local terminal.
	})
	if err != nil {
		return fmt.Errorf("shell on %s: %w", name, err)
	}
	// Propagate the remote exit code so a wrapping script sees the
	// truth of "did the remote shell exit cleanly?"
	if exit != 0 {
		os.Exit(exit)
	}
	return nil
}

func sshTunnelCmd() *cobra.Command {
	var (
		jumpFlag    string
		localFwdArg string
	)
	cmd := &cobra.Command{
		Use:   "tunnel <name> -L <local>:<remote-host>:<remote-port>",
		Short: "Open a local port forward through a configured target",
		Long: `Opens a local listener on 127.0.0.1:<local> and bridges every
accepted connection to <remote-host>:<remote-port> on the remote
outpost's reachable network via an SSH direct-tcpip channel.

The listener stays up until Ctrl-C; the in-process client tears
down each forwarded conn when either side closes.

Example:
  outpost ssh tunnel lab -L 5432:db.lan:5432
  psql -h 127.0.0.1 -p 5432 ...

--jump <alias> overrides the target's persisted Via for this one
call (analogous to ssh's -J).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			localPort, destHost, destPort, err := parseLocalForwardArg(localFwdArg)
			if err != nil {
				return err
			}
			return runSSHTunnel(cmd.Context(), args[0], jumpFlag, localPort, destHost, destPort)
		},
	}
	cmd.Flags().StringVarP(&localFwdArg, "local-forward", "L", "",
		"local:dest_host:dest_port — same format as ssh -L")
	cmd.Flags().StringVar(&jumpFlag, "jump", "",
		"ProxyJump alias for this call (overrides the target's persisted --via)")
	_ = cmd.MarkFlagRequired("local-forward")
	return cmd
}

// parseLocalForwardArg parses ssh's -L syntax: local:host:port.
// The local port can also be IP:port if the operator wants to bind
// off-loopback, but our default is 127.0.0.1.
func parseLocalForwardArg(s string) (localPort int, destHost string, destPort int, err error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, "", 0, errors.New("--local-forward must be local:host:port")
	}
	lp, e := strconv.Atoi(parts[0])
	if e != nil || lp <= 0 || lp > 65535 {
		return 0, "", 0, fmt.Errorf("invalid local port %q", parts[0])
	}
	dh := strings.TrimSpace(parts[1])
	if dh == "" {
		return 0, "", 0, errors.New("destination host is empty")
	}
	dp, e := strconv.Atoi(parts[2])
	if e != nil || dp <= 0 || dp > 65535 {
		return 0, "", 0, fmt.Errorf("invalid destination port %q", parts[2])
	}
	return lp, dh, dp, nil
}

func runSSHTunnel(ctx context.Context, name, jump string, localPort int, destHost string, destPort int) error {
	client, cleanup, err := dialSSHTargetChain(ctx, name, jump)
	if err != nil {
		return err
	}
	defer cleanup()

	addr := fmt.Sprintf("127.0.0.1:%d", localPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	fmt.Fprintf(os.Stderr, "outpost ssh tunnel: forwarding %s -> %s:%d via %s. Ctrl-C to exit.\n",
		addr, destHost, destPort, name)
	// Blocks until ctx cancellation (signal handler at process root) or
	// the listener errors.
	if err := client.LocalForward(ctx, listener, destHost, destPort); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
	return nil
}

func sshSFTPCmd() *cobra.Command {
	var jumpFlag string
	cmd := &cobra.Command{
		Use:   "sftp <name>",
		Short: "File transfer over SFTP to a configured target",
		Long: `Subcommands:
  outpost ssh sftp <name> ls <remote-path>
  outpost ssh sftp <name> get <remote-path> [local-path]
  outpost ssh sftp <name> put <local-path> [remote-path]

--jump <alias> overrides the target's persisted Via for this one
call (analogous to ssh's -J).`,
	}
	cmd.PersistentFlags().StringVar(&jumpFlag, "jump", "",
		"ProxyJump alias for this call (overrides the target's persisted --via)")
	cmd.AddCommand(
		sshSFTPLsCmd(&jumpFlag),
		sshSFTPGetCmd(&jumpFlag),
		sshSFTPPutCmd(&jumpFlag),
	)
	return cmd
}

func sshSFTPLsCmd(jump *string) *cobra.Command {
	return &cobra.Command{
		Use:   "ls <name> <remote-path>",
		Short: "List a remote directory",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSFTP(cmd.Context(), args[0], *jump, func(c *sftp.Client) error {
				entries, err := c.ReadDir(args[1])
				if err != nil {
					return fmt.Errorf("readdir: %w", err)
				}
				for _, e := range entries {
					fmt.Printf("%s\t%d\t%s\n", e.Mode(), e.Size(), e.Name())
				}
				return nil
			})
		},
	}
}

func sshSFTPGetCmd(jump *string) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name> <remote-path> [local-path]",
		Short: "Download a remote file (writes to stdout when local-path is omitted)",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			remote := args[1]
			var local string
			if len(args) == 3 {
				local = args[2]
			}
			return withSFTP(cmd.Context(), args[0], *jump, func(c *sftp.Client) error {
				rf, err := c.Open(remote)
				if err != nil {
					return fmt.Errorf("open remote: %w", err)
				}
				defer rf.Close()
				var dst io.Writer = os.Stdout
				if local != "" {
					out, err := os.Create(local)
					if err != nil {
						return fmt.Errorf("create local: %w", err)
					}
					defer out.Close()
					dst = out
				}
				n, err := io.Copy(dst, rf)
				if err != nil {
					return err
				}
				if local != "" {
					fmt.Fprintf(os.Stderr, "got %d bytes into %s\n", n, local)
				}
				return nil
			})
		},
	}
}

func sshSFTPPutCmd(jump *string) *cobra.Command {
	return &cobra.Command{
		Use:   "put <name> <local-path> [remote-path]",
		Short: "Upload a local file (uses basename when remote-path is omitted)",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			local := args[1]
			remote := ""
			if len(args) == 3 {
				remote = args[2]
			} else {
				remote = filepath.Base(local)
			}
			return withSFTP(cmd.Context(), args[0], *jump, func(c *sftp.Client) error {
				lf, err := os.Open(local)
				if err != nil {
					return fmt.Errorf("open local: %w", err)
				}
				defer lf.Close()
				rf, err := c.Create(remote)
				if err != nil {
					return fmt.Errorf("create remote: %w", err)
				}
				defer rf.Close()
				n, err := io.Copy(rf, lf)
				if err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "put %d bytes into %s\n", n, remote)
				return nil
			})
		},
	}
}

// withSFTP opens the chain, creates a *sftp.Client, runs fn, and
// tears down. Used by each sftp sub-subcommand to keep the dial
// plumbing in one place.
func withSFTP(ctx context.Context, name, jump string, fn func(*sftp.Client) error) error {
	client, cleanup, err := dialSSHTargetChain(ctx, name, jump)
	if err != nil {
		return err
	}
	defer cleanup()
	sc, err := client.SFTP()
	if err != nil {
		return fmt.Errorf("open sftp: %w", err)
	}
	defer sc.Close()
	return fn(sc)
}

// Note: sshclient is imported indirectly via ssh_dial.go's
// dialSSHTargetChain; the bare reference below keeps the import
// list honest for future Connect/Shell options.
var _ = sshclient.ShellOptions{}
