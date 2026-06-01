// SSH client-side helpers: `outpost ssh-proxy` (used as an SSH
// ProxyCommand to bridge a local `ssh` invocation to the remote outpost
// over the matrix tunnel) and `outpost ssh-config` (prints
// ~/.ssh/config stanzas for hosts visible to this account).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/outpost/internal/agent/sshclient"
)

func sshProxyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ssh-proxy <host>",
		Short: "Bridge stdin/stdout to wss://<cloudbox>/h/<host>/ssh (use as an SSH ProxyCommand)",
		Long: `ssh-proxy is invoked by your local ssh client through ~/.ssh/config:

    Host myhome
        User myuser
        ProxyCommand outpost ssh-proxy %h

It reads the local outpost's saved config, opens a WebSocket to
cloudbox's /h/<host>/ssh endpoint with the persisted bearer token,
and pipes stdin <-> WebSocket <-> stdout. The remote outpost answers
the SSH protocol on the other side.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSSHProxy(cmd.Context(), args[0])
		},
	}
}

func runSSHProxy(ctx context.Context, host string) error {
	cfgPath, err := conf.DefaultConfigPath()
	if err != nil {
		return fmt.Errorf("locate config: %w", err)
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if fc == nil || fc.ServerAddr == "" {
		return errors.New("local outpost is not paired with cloudbox yet — run `outpost register` first")
	}
	// Auth source for cloudbox's /h/:host/ssh route, in priority order:
	//   1. $OUTPOST_SESSION_JWT — operator-supplied user JWT (debug).
	//   2. fc.AccessToken — per-user scoped JWT cloudbox issued at
	//      register time. The normal path for paired outposts.
	//   3. fc.Token — matrix-tunnel shared secret. Kept as a back-stop
	//      for outposts paired against older cloudboxes that did not
	//      yet return access_token.
	bearer := strings.TrimSpace(os.Getenv("OUTPOST_SESSION_JWT"))
	if bearer == "" {
		bearer = fc.AccessToken
	}
	if bearer == "" {
		bearer = fc.Token
	}
	if bearer == "" {
		return errors.New("no auth credential: re-pair with `outpost register` against a cloudbox that returns access_token, or set OUTPOST_SESSION_JWT to a user session JWT")
	}

	wsURL, err := sshclient.BuildWSURL(fc.ServerAddr, fc.ServerPort, fc.Protocol, host)
	if err != nil {
		return err
	}

	conn, err := dialSSHWS(ctx, wsURL, bearer, host)
	if err != nil {
		return err
	}
	// Allow virtually-unlimited message sizes; ssh streams can carry large
	// scp/rsync transfers.
	conn.SetReadLimit(-1)

	netConn := websocket.NetConn(ctx, conn, websocket.MessageBinary)
	defer netConn.Close()

	return pipeSSHProxy(ctx, conn, netConn, os.Stdin, os.Stdout)
}

// pipeSSHProxy wires a duplex byte stream between the local SSH client
// (in/out) and the remote outpost's /ssh endpoint (netConn). The
// session is considered alive until the SERVER side ends — i.e. until
// netConn.Read returns. Local stdin closing is treated as
// "client stopped writing" only, not as session end.
//
// Why the asymmetry: SSH terminates via in-protocol CHANNEL_CLOSE
// messages that arrive on the netConn read side. Some parent shells
// and pty wrappers (agentic harnesses, ssh ProxyCommand under
// non-standard $SHELL) expose an early EOF or non-EOF error on stdin
// even when SSH is mid-handshake. Under a naive "first-side-to-end
// terminates" design, that tore down the WS before the server's
// KEX_REPLY could come back — visible as "Connection closed by
// UNKNOWN port 65535" right after the client's KEXINIT. Splitting
// netConn (authoritative) from stdin (advisory) makes the proxy
// robust to those parent-shell quirks.
//
// conn is exposed separately from netConn so we can issue a clean
// CloseFrame on exit; the netConn wrapper only supports byte-level
// close.
func pipeSSHProxy(ctx context.Context, conn *websocket.Conn, netConn net.Conn, stdin io.Reader, stdout io.Writer) error {
	errStdout := make(chan error, 1)
	go func() {
		_, e := io.Copy(stdout, netConn)
		errStdout <- e
	}()
	go func() {
		// Fire-and-forget: an early/spurious EOF or error here must
		// NOT terminate the session. See function-level comment.
		// The deferred netConn.Close() in the caller unblocks any
		// in-flight Read when the function returns.
		_, _ = io.Copy(netConn, stdin)
	}()

	select {
	case <-ctx.Done():
		_ = conn.Close(websocket.StatusGoingAway, "ctx done")
		return ctx.Err()
	case e := <-errStdout:
		_ = conn.Close(websocket.StatusNormalClosure, "")
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		return nil
	}
}

// dialSSHWS is the cmd/outpost-side façade over sshclient.DialWS — it
// adds the interactive recovery callback that re-elevates by prompting
// the human at /dev/tty (calling runConnect). The transport mechanics
// (bearer header, cookie attach, 401/403 retry) live in the shared
// sshclient package so the in-process SSH client (`outpost ssh exec`,
// MCP `outpost_ssh_exec`) reuses the same dial.
func dialSSHWS(ctx context.Context, wsURL, bearer, host string) (*websocket.Conn, error) {
	cookie, _ := readCookie(host)
	onElevate := func(ctx context.Context, h string) (string, error) {
		// Agents call `outpost connect` themselves; only prompt when
		// there's a real human at the keyboard.
		if !haveTTY() {
			return "", errors.New("no TTY for password prompt")
		}
		fmt.Fprintf(os.Stderr, "outpost: %s requires Connect; prompting for OS password…\n", h)
		// Interactive recovery uses cloudbox's default TTL — the
		// operator just wants the session back. Pass --ttl on the
		// outer `outpost connect` for a long-lived override.
		if err := runConnect(ctx, h, "", false, false, 0); err != nil {
			return "", err
		}
		fresh, _ := readCookie(h)
		return fresh, nil
	}
	return sshclient.DialWS(ctx, sshclient.DialOptions{
		WSURL:     wsURL,
		Bearer:    bearer,
		Cookie:    cookie,
		Host:      host,
		OnElevate: onElevate,
	})
}

func haveTTY() bool {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func sshConfigCmd() *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "ssh-config",
		Short: "Print ~/.ssh/config stanzas for hosts visible to this account",
		Long: `Queries cloudbox for the hosts you can reach (your own + shared
with you) and prints one ~/.ssh/config stanza per host using
outpost ssh-proxy as the ProxyCommand.

v1 is print-and-paste — copy the output into your ~/.ssh/config and
you can then run "ssh <host>" against any of them. --apply (planned)
will merge into ~/.ssh/config in-place.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if apply {
				return errors.New("--apply is not implemented yet; copy the printed stanzas into ~/.ssh/config by hand")
			}
			return runSSHConfig(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "Merge stanzas into ~/.ssh/config in-place (not yet implemented)")
	return cmd
}

// sshHostEntry is the subset of cloudbox's /api/v1/ssh/hosts response we
// care about. Defined locally so this command stays decoupled from any
// internal cloudbox types.
type sshHostEntry struct {
	Host   string `json:"host"`    // outpost agent name, used in the WSS URL
	OsUser string `json:"os_user"` // the OS user the remote outpost runs as
}

func runSSHConfig(ctx context.Context) error {
	cfgPath, err := conf.DefaultConfigPath()
	if err != nil {
		return err
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		return err
	}
	if fc == nil || fc.ServerAddr == "" {
		return errors.New("local outpost is not paired with cloudbox yet — run `outpost register` first")
	}
	// Same auth-source preference as runSSHProxy: env override, then
	// the per-user access_token cloudbox issued at register time, then
	// the matrix-tunnel shared secret as a last-resort back-stop.
	bearer := strings.TrimSpace(os.Getenv("OUTPOST_SESSION_JWT"))
	if bearer == "" {
		bearer = fc.AccessToken
	}
	if bearer == "" {
		bearer = fc.Token
	}

	hosts, err := fetchSSHHosts(ctx, fc.ServerAddr, fc.ServerPort, fc.Protocol, bearer)
	if err != nil {
		// Fall back to printing a single stanza for the local outpost so
		// users can at least self-ssh while cloudbox catches up to the
		// /api/v1/ssh/hosts endpoint.
		fmt.Fprintf(os.Stderr, "warning: could not list hosts from cloudbox (%v); printing self-stanza only.\n", err)
		osUser, _ := hostauth.CurrentUser()
		hosts = []sshHostEntry{{Host: fc.AgentName, OsUser: osUser}}
	}

	exe, _ := os.Executable()
	if exe == "" {
		exe = "outpost"
	}
	for _, h := range hosts {
		if h.Host == "" {
			continue
		}
		fmt.Printf("Host %s\n", h.Host)
		if h.OsUser != "" {
			fmt.Printf("    User %s\n", h.OsUser)
		}
		fmt.Printf("    ProxyCommand %s ssh-proxy %%h\n", exe)
		fmt.Printf("    HostKeyAlias outpost-%s\n", h.Host)
		fmt.Printf("    ServerAliveInterval 30\n")
		fmt.Println()
	}
	return nil
}

func fetchSSHHosts(ctx context.Context, server string, port int, protocol, token string) ([]sshHostEntry, error) {
	s := strings.TrimSpace(server)
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	// Mirror the same scheme heuristic ssh-proxy uses.
	if strings.EqualFold(strings.TrimSpace(protocol), "wss") || strings.EqualFold(u.Scheme, "https") {
		u.Scheme = "https"
	} else {
		u.Scheme = "http"
	}
	if u.Port() == "" && port > 0 {
		u.Host = u.Hostname() + ":" + strconv.Itoa(port)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/ssh/hosts"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Hosts []sshHostEntry `json:"hosts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return out.Hosts, nil
}
