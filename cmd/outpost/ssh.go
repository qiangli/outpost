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
	"github.com/qiangli/yoke/pkg/sshclient"
)

func sshProxyCmd() *cobra.Command {
	var modeFlag string
	var directPortFlag int
	cmd := &cobra.Command{
		Use:   "ssh-proxy <host>",
		Short: "Bridge stdin/stdout to <host>'s sshd directly when reachable, else via the cloudbox relay (use as an SSH ProxyCommand)",
		Long: `ssh-proxy is invoked by your local ssh client through ~/.ssh/config:

    Host myhome
        User myuser
        ProxyCommand outpost ssh-proxy %h

It reads the local outpost's saved config, opens a WebSocket to
cloudbox's /matrix/h/<host>/ssh endpoint with the persisted bearer
token, and pipes stdin <-> WebSocket <-> stdout. The remote outpost
answers the SSH protocol on the other side.

Direct-first: before touching the relay, ssh-proxy tries a plain
TCP dial to the host's LAN endpoint(s) — the name as given plus its
mDNS "<host>.local" form, on ports 22 then 2222 — with a short
connect timeout. When one connects, stdin/stdout are piped straight
through, so the user's own keys and HostKeyAlias keep working with
no outpost/cloudbox involvement. The relay (and any elevation
prompt, which only ever happens at /dev/tty) runs solely as the
fallback when no direct path connects.

$OUTPOST_SSH_PROXY_MODE / --mode selects the policy: auto
(default), direct (never use the relay), or relay (never try
direct). $OUTPOST_SSH_DIRECT_PORT / --direct-port pins a single
LAN sshd port instead of probing the defaults.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSSHProxy(cmd.Context(), args[0], sshProxyOptions{mode: modeFlag, directPort: directPortFlag})
		},
	}
	cmd.Flags().StringVar(&modeFlag, "mode", "", "Direct-vs-relay policy: auto (default), direct, or relay. Overrides $OUTPOST_SSH_PROXY_MODE.")
	cmd.Flags().IntVar(&directPortFlag, "direct-port", 0, "Single LAN sshd port to try for the direct path (default: try 22 then 2222). Overrides $OUTPOST_SSH_DIRECT_PORT.")
	return cmd
}

// sshProxyOptions carries the direct-vs-relay escape hatches. The
// zero value is the default policy: auto with probed ports.
type sshProxyOptions struct {
	mode       string
	directPort int
}

// sshProxyDirectTimeout bounds each direct-TCP candidate dial. All
// candidates dial concurrently, so it also bounds the whole direct
// attempt: the relay fallback starts at most ~2 s after a LAN miss.
const sshProxyDirectTimeout = 2 * time.Second

// sshProxyDefaultDirectPorts are the LAN sshd ports probed when no
// explicit port is configured: the system sshd first (the user's
// own key auth lives here), then the outpost sshd default.
var sshProxyDefaultDirectPorts = []int{22, 2222}

// sshProxyMode normalizes the CLI flag / env escape hatch to one of
// "auto", "direct", or "relay". Unknown values fall back to auto.
func sshProxyMode(flagVal string) string {
	m := strings.ToLower(strings.TrimSpace(flagVal))
	if m == "" {
		m = strings.ToLower(strings.TrimSpace(os.Getenv("OUTPOST_SSH_PROXY_MODE")))
	}
	switch m {
	case "direct", "relay":
		return m
	default:
		return "auto"
	}
}

// sshProxyDirectPort resolves the single-port override: the CLI flag
// wins, then $OUTPOST_SSH_DIRECT_PORT, else 0 (probe the defaults).
func sshProxyDirectPort(flagVal int) int {
	if flagVal > 0 && flagVal <= 65535 {
		return flagVal
	}
	if v := strings.TrimSpace(os.Getenv("OUTPOST_SSH_DIRECT_PORT")); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p <= 65535 {
			return p
		}
	}
	return 0
}

// sshProxyCandidates lists the direct-TCP endpoints for host, in
// preference order: the name as given (LAN DNS / literal IP), then
// its mDNS "<host>.local" form, on each port. An explicit port pins
// a single port; otherwise the default ports apply. A host that
// already carries a host:port, an IP literal (no .local form), or a
// .local name (not duplicated) yields the obvious smaller set.
func sshProxyCandidates(host string, port int) []string {
	h := strings.TrimSpace(host)
	if h == "" {
		return nil
	}
	if hh, pp, err := net.SplitHostPort(h); err == nil && hh != "" && pp != "" {
		return []string{h}
	}
	ports := []int{port}
	if port <= 0 {
		ports = sshProxyDefaultDirectPorts
	}
	names := []string{h}
	if net.ParseIP(h) == nil && !strings.HasSuffix(strings.ToLower(h), ".local") {
		names = append(names, h+".local")
	}
	var out []string
	seen := map[string]struct{}{}
	for _, p := range ports {
		if p <= 0 || p > 65535 {
			continue
		}
		for _, n := range names {
			addr := net.JoinHostPort(n, strconv.Itoa(p))
			if _, dup := seen[addr]; !dup {
				seen[addr] = struct{}{}
				out = append(out, addr)
			}
		}
	}
	return out
}

// dialSSHProxyDirect dials every candidate concurrently and returns
// the first TCP connection that succeeds — plain TCP pass-through,
// so the ssh client keeps owning the handshake, key auth, and
// HostKeyAlias verification. When every candidate fails it returns
// an error naming each attempt, so the caller can fold it into the
// relay failure as one message naming both paths.
func dialSSHProxyDirect(ctx context.Context, candidates []string, timeout time.Duration) (net.Conn, error) {
	if len(candidates) == 0 {
		return nil, errors.New("no direct candidates")
	}
	if timeout <= 0 {
		timeout = sshProxyDirectTimeout
	}
	type result struct {
		conn net.Conn
		addr string
		err  error
	}
	resCh := make(chan result, len(candidates))
	dialer := &net.Dialer{Timeout: timeout}
	for _, addr := range candidates {
		go func(a string) {
			c, e := dialer.DialContext(ctx, "tcp", a)
			resCh <- result{conn: c, addr: a, err: e}
		}(addr)
	}
	var fails []string
	for i := 0; i < len(candidates); i++ {
		r := <-resCh
		if r.err == nil && r.conn != nil {
			// First success wins; late winners are closed by a
			// background drain so concurrent dials never leak a
			// socket (the buffered channel never blocks them).
			if remaining := len(candidates) - i - 1; remaining > 0 {
				go func() {
					for j := 0; j < remaining; j++ {
						if late := <-resCh; late.conn != nil {
							_ = late.conn.Close()
						}
					}
				}()
			}
			return r.conn, nil
		}
		if r.err != nil {
			fails = append(fails, r.addr+": "+r.err.Error())
		} else {
			fails = append(fails, r.addr+": closed before use")
		}
	}
	return nil, errors.New(strings.Join(fails, "; "))
}

// pipeSSHProxyTCP pipes stdio straight to a direct-LAN TCP
// connection: the local ssh client owns the session, so this is a
// dumb byte pipe. It mirrors pipeSSHProxy's asymmetry — the sshd
// side ending the stream ends the session, while stdin EOF alone
// only stops the upload direction.
func pipeSSHProxyTCP(ctx context.Context, conn net.Conn, stdin io.Reader, stdout io.Writer) error {
	errOut := make(chan error, 1)
	go func() {
		_, e := io.Copy(stdout, conn)
		errOut <- e
	}()
	go func() {
		_, _ = io.Copy(conn, stdin)
	}()

	select {
	case <-ctx.Done():
		_ = conn.Close()
		return ctx.Err()
	case e := <-errOut:
		_ = conn.Close()
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		return nil
	}
}

// Seams the focused ssh-proxy tests stub: the direct-TCP leg and
// the whole relay leg. Production code uses the real functions.
var (
	sshProxyDialDirect = dialSSHProxyDirect
	sshProxyRunRelay   = runSSHProxyRelay
)

func runSSHProxy(ctx context.Context, host string, opts sshProxyOptions) error {
	return runSSHProxyWithIO(ctx, host, opts, os.Stdin, os.Stdout)
}

// runSSHProxyWithIO is the direct-first ssh-proxy core: try the LAN
// TCP path, fall back to the cloudbox relay, and — when headless —
// fail fast with one error naming both attempts instead of ever
// prompting for a password. stdin/stdout are parameters (not
// os.Stdin/os.Stdout directly) so tests can drive the byte flow.
func runSSHProxyWithIO(ctx context.Context, host string, opts sshProxyOptions, stdin io.Reader, stdout io.Writer) error {
	mode := sshProxyMode(opts.mode)
	port := sshProxyDirectPort(opts.directPort)
	var (
		tried     []string
		directErr error
	)
	if mode == "auto" || mode == "direct" {
		tried = sshProxyCandidates(host, port)
		if conn, derr := sshProxyDialDirect(ctx, tried, sshProxyDirectTimeout); derr == nil {
			fmt.Fprintf(os.Stderr, "outpost: ssh-proxy %s: direct LAN path via %s\n", host, conn.RemoteAddr())
			return pipeSSHProxyTCP(ctx, conn, stdin, stdout)
		} else {
			directErr = derr
		}
		if mode == "direct" {
			return fmt.Errorf("ssh-proxy %s: direct LAN path failed (tried %s): %v (relay disabled by mode=direct)", host, strings.Join(tried, ", "), directErr)
		}
	}
	rerr := sshProxyRunRelay(ctx, host, stdin, stdout)
	if rerr == nil {
		return nil
	}
	// Headless safety: the relay leg only ever prompts at /dev/tty
	// (dialSSHWS refuses without one), so reaching here headless
	// already means "no prompt happened" — fold the direct misses
	// in so the operator sees both paths in one message.
	if directErr != nil {
		return fmt.Errorf("ssh-proxy %s: no direct LAN path (tried %s: %v); relay failed: %w", host, strings.Join(tried, ", "), directErr, rerr)
	}
	return rerr
}

// runSSHProxyRelay is the pre-direct-first ssh-proxy behavior,
// unchanged: bridge stdio to cloudbox's /matrix/h/<host>/ssh
// endpoint over WSS. Elevation prompts (if any) happen at /dev/tty
// via dialSSHWS — never on the stdin pipe, which carries SSH bytes.
func runSSHProxyRelay(ctx context.Context, host string, stdin io.Reader, stdout io.Writer) error {
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

	return pipeSSHProxy(ctx, conn, netConn, stdin, stdout)
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
	Shared bool   `json:"shared"`  // true when the caller does not own the host (share grant)
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
