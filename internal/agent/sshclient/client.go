// In-process SSH client over the cloudbox matrix tunnel.
//
// The remote outpost's `internal/agent/ssh.go` wraps every incoming
// WS connection as a net.Conn and feeds it to ssh.NewServerConn —
// the SSH protocol runs end-to-end through the byte pipe. This
// client mirrors that: dial the WS, wrap it as a net.Conn, hand the
// conn to ssh.NewClientConn. The result is a full Go SSH client that
// reaches paired hosts without any system /usr/bin/ssh involvement.
//
// Authentication: when cloudbox stamps `X-Periscope-Role: user|admin`
// on the WS upgrade (which it does whenever the matrix_elev cookie
// passes the elevation gate), the remote SSH server flips into
// NoClientAuth mode — the OS-password challenge is skipped because
// the WS-layer auth already proved the operator's intent. We
// therefore pass no AuthMethods on the client side. If the WS
// handshake itself fails 401/403 the dial returns EAuthRequiredError
// before we get this far.
//
// Wave 1 of the SSH self-sufficiency work supports Exec only — one-
// shot remote commands. Interactive shell, SFTP, and port-forwarding
// are Wave 2 additions on the same Client surface.
package sshclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// Client wraps an established SSH connection (over the matrix tunnel)
// for one paired host.
type Client struct {
	// ssh is the established connection — used for OpenChannel /
	// SendRequest.
	ssh *ssh.Client

	// closers run on Close in reverse order: SSH conn, WS netConn,
	// raw WS conn. Keeping them as a slice avoids the field-by-field
	// nil-check dance.
	closers []func() error
}

// Config wires the dial-time parameters. Almost all callers will fill
// this from a conf.SSHTarget plus the FileConfig the daemon already
// has cached.
type Config struct {
	// Transport is the byte pipe to the remote SSH server. Typically
	// the result of websocket.NetConn(...) wrapping a *websocket.Conn
	// returned by DialWS. The caller owns the underlying *websocket.Conn
	// — we Close the net.Conn (which closes the WS) but don't reach
	// into the WS API directly.
	Transport net.Conn

	// HostAlias is the canonical alias used for host-key pinning. The
	// `outpost-<host>` form matches what `outpost ssh-config` emits.
	HostAlias string

	// User is the OS username to log in as on the remote host. Empty
	// is rejected — let the caller resolve from cloudbox's
	// /api/v1/ssh/hosts before getting here.
	User string

	// HostKeyCallback verifies (and on first connect, pins) the
	// remote outpost's host key. Build via KnownHostsCallbackTOFU.
	HostKeyCallback ssh.HostKeyCallback

	// Auth supplies client-side SSH auth methods. Nil keeps the
	// historical behavior (no methods): the cloudbox-vouched paths rely
	// on the remote server flipping NoClientAuth=true, so the handshake
	// completes without credentials. LAN-direct dials (plain TCP to an
	// `outpost sshd` / SSHListenAddr listener) have no upstream vouching
	// — the server runs its OS-password gate — so those callers pass a
	// password method here (typically ssh.RetryableAuthMethod wrapping
	// a TTY prompt).
	Auth []ssh.AuthMethod

	// HandshakeTimeout caps the SSH transport handshake. Default 30s.
	HandshakeTimeout time.Duration
}

// Dial runs the SSH handshake over an already-established transport.
// Caller is responsible for the WS dial (DialWS); this just layers
// SSH protocol on top.
//
// On success, the returned Client owns the transport — Close() will
// tear it down.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Transport == nil {
		return nil, errors.New("sshclient: nil Transport")
	}
	if cfg.User == "" {
		return nil, errors.New("sshclient: empty User (resolve OS username before dialing)")
	}
	if cfg.HostKeyCallback == nil {
		return nil, errors.New("sshclient: nil HostKeyCallback (use KnownHostsCallbackTOFU)")
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = 30 * time.Second
	}

	sshCfg := &ssh.ClientConfig{
		User: cfg.User,
		// Default nil AuthMethods: the remote /ssh server flips
		// NoClientAuth=true when cloudbox stamps X-Periscope-Role on the
		// WS upgrade — see internal/agent/ssh.go's PasswordCallback
		// branch. The matrix tunnel + elev cookie is the auth boundary.
		// LAN-direct callers (no vouching available) pass cfg.Auth so
		// the server's OS-password gate can be satisfied in-band.
		Auth:            cfg.Auth,
		HostKeyCallback: cfg.HostKeyCallback,
		Timeout:         cfg.HandshakeTimeout,
	}

	// The hostname passed to NewClientConn is informational — it ends
	// up in the SSH handshake's banner / log lines but is not the
	// trust anchor (HostKeyCallback handles that). Use the alias for
	// consistency with the known_hosts entries.
	target := cfg.HostAlias
	if target == "" {
		target = "outpost-host"
	}

	// Honor ctx during the handshake by closing the transport when
	// the ctx is canceled. ssh.NewClientConn blocks; without this
	// path a stuck handshake would ignore caller cancellation.
	handshakeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = cfg.Transport.Close()
		case <-handshakeDone:
		}
	}()

	sshConn, chans, reqs, err := ssh.NewClientConn(cfg.Transport, target, sshCfg)
	close(handshakeDone)
	if err != nil {
		_ = cfg.Transport.Close()
		return nil, fmt.Errorf("ssh handshake to %s: %w", target, err)
	}

	client := ssh.NewClient(sshConn, chans, reqs)
	return &Client{
		ssh: client,
		closers: []func() error{
			client.Close,
			cfg.Transport.Close,
		},
	}, nil
}

// Close tears down the SSH conn and the underlying transport in the
// right order. Idempotent — repeated Close calls return nil after
// the first.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	var firstErr error
	for i := len(c.closers) - 1; i >= 0; i-- {
		if err := c.closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	c.closers = nil
	return firstErr
}

// ExecResult is the structured outcome of a one-shot exec.
type ExecResult struct {
	// Stdout is the (possibly truncated) standard output.
	Stdout []byte

	// Stderr is the (possibly truncated) standard error.
	Stderr []byte

	// ExitCode is the remote process's exit status. 0 on success;
	// the actual non-zero code when the remote returned one;
	// -1 when the remote signaled the process (no exit code) or
	// when ssh.Session.Wait returned a non-ExitError.
	ExitCode int

	// StdoutTruncated / StderrTruncated indicate the corresponding
	// stream hit the LimitReader cap. The cap is set via Exec's
	// MaxStdout / MaxStderr fields.
	StdoutTruncated bool
	StderrTruncated bool
}

// ExecOptions tunes one-shot remote execution.
type ExecOptions struct {
	// Command is the literal command line (joined as the SSH server
	// would see it). Quoting / escaping is the caller's job.
	Command string

	// Timeout terminates the exec if the remote takes longer. 0 means
	// "use the parent context's deadline only."
	Timeout time.Duration

	// MaxStdout / MaxStderr cap the captured output. Anything past the
	// limit is discarded and the corresponding Truncated flag is set.
	// 0 means use the defaults (1 MiB stdout, 256 KiB stderr).
	MaxStdout int64
	MaxStderr int64

	// Stdin is fed to the remote process. Nil = empty stdin (most
	// agentic callers want this). Closed when copy completes.
	Stdin io.Reader
}

// Exec runs cmd on the remote host and returns its stdout/stderr +
// exit code. Suitable for the MCP `outpost_ssh_exec` tool and the
// CLI `outpost ssh exec <name> -- <cmd>` subcommand.
func (c *Client) Exec(ctx context.Context, opts ExecOptions) (*ExecResult, error) {
	if c == nil || c.ssh == nil {
		return nil, errors.New("sshclient: nil Client")
	}
	if opts.Command == "" {
		return nil, errors.New("sshclient: empty Command")
	}
	if opts.MaxStdout <= 0 {
		opts.MaxStdout = 1 << 20 // 1 MiB
	}
	if opts.MaxStderr <= 0 {
		opts.MaxStderr = 256 << 10 // 256 KiB
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	sess, err := c.ssh.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open session: %w", err)
	}
	defer sess.Close()

	var (
		outBuf bytes.Buffer
		errBuf bytes.Buffer
	)
	// LimitReader-style truncation: copy up to limit+1 bytes; if we
	// hit limit+1, we know there was more.
	sess.Stdout = &cappedWriter{w: &outBuf, limit: opts.MaxStdout}
	sess.Stderr = &cappedWriter{w: &errBuf, limit: opts.MaxStderr}
	if opts.Stdin != nil {
		sess.Stdin = opts.Stdin
	}

	// Honor ctx by closing the session when ctx fires. ssh.Session.Wait
	// returns when the channel closes, which happens on Close().
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Close()
		case <-done:
		}
	}()

	runErr := sess.Run(opts.Command)
	close(done)

	res := &ExecResult{
		Stdout:          outBuf.Bytes(),
		Stderr:          errBuf.Bytes(),
		StdoutTruncated: sess.Stdout.(*cappedWriter).truncated,
		StderrTruncated: sess.Stderr.(*cappedWriter).truncated,
	}

	switch {
	case runErr == nil:
		res.ExitCode = 0
	case isCtxErr(ctx):
		return res, fmt.Errorf("exec timed out: %w", ctx.Err())
	default:
		var exitErr *ssh.ExitError
		if errors.As(runErr, &exitErr) {
			res.ExitCode = exitErr.ExitStatus()
		} else {
			// ssh.ExitMissingError (clean disconnect, no exit) or any
			// other transport error: convey via ExitCode=-1.
			res.ExitCode = -1
			return res, fmt.Errorf("exec failed: %w", runErr)
		}
	}
	return res, nil
}

// cappedWriter discards bytes past `limit` and remembers whether it
// truncated. The trim happens silently from the writer's perspective —
// the SSH session keeps streaming until the remote finishes; we just
// stop accumulating after the cap.
type cappedWriter struct {
	w         *bytes.Buffer
	limit     int64
	written   int64
	truncated bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.written >= c.limit {
		c.truncated = true
		return len(p), nil // pretend we wrote it; drop on the floor
	}
	remain := c.limit - c.written
	if int64(len(p)) <= remain {
		n, err := c.w.Write(p)
		c.written += int64(n)
		return n, err
	}
	// Partial: write what we can, drop the rest.
	n, err := c.w.Write(p[:remain])
	c.written += int64(n)
	c.truncated = true
	return len(p), err
}

func isCtxErr(ctx context.Context) bool {
	return ctx.Err() != nil
}

// AsNetConn is a small convenience wrapper for callers that already
// have a *websocket.Conn from DialWS and want the canonical net.Conn
// form to feed into Dial. Equivalent to websocket.NetConn(ctx, conn,
// websocket.MessageBinary) — exposed here so callers don't need to
// import coder/websocket directly when sshclient is sufficient.
func AsNetConn(ctx context.Context, conn *websocket.Conn) net.Conn {
	return websocket.NetConn(ctx, conn, websocket.MessageBinary)
}

// DirectTCPIP opens an SSH "direct-tcpip" channel through this Client
// to (host, port) on the remote outpost's reachable network. The
// returned net.Conn byte-bridges through the channel — suitable for
// either layering another SSH session on top (the hop/ProxyJump
// pattern) or for plain TCP forwarding (the tunnel pattern).
//
// host is the destination as the remote outpost would resolve it.
// For paired-outpost-to-paired-outpost hops this is a peer hostname
// the remote's peerhosts allowlist accepts. For LAN destinations it
// must match an entry the operator added to SSHForwardSockets or
// otherwise widened the destination allowlist for.
//
// The caller owns the returned conn — close it (or the parent
// Client) to tear down the channel.
func (c *Client) DirectTCPIP(ctx context.Context, host string, port int) (net.Conn, error) {
	if c == nil || c.ssh == nil {
		return nil, errors.New("sshclient: nil Client")
	}
	if port <= 0 {
		return nil, fmt.Errorf("sshclient: invalid port %d", port)
	}
	conn, err := c.ssh.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port)))
	if err != nil {
		return nil, fmt.Errorf("direct-tcpip to %s:%d: %w", host, port, err)
	}
	return conn, nil
}

// ShellOptions parameterizes an interactive shell session.
type ShellOptions struct {
	// Stdin/Stdout/Stderr wire the local terminal to the remote PTY.
	// Typically os.Stdin / os.Stdout / os.Stderr. Stderr is merged
	// into Stdout by the SSH PTY by default; we keep the field for
	// callers that want to split (uncommon).
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// TermType is the TERM string sent in pty-req (default "xterm-256color").
	TermType string

	// Width / Height seed the remote PTY's dimensions. When zero and
	// Stdin is a TTY, the dimensions are autodetected from the
	// terminal. SIGWINCH-driven updates take effect regardless.
	Width  int
	Height int
}

// Shell opens a session, requests a PTY, starts the user's login
// shell, and pipes I/O until the session ends. Handles:
//   - Local terminal raw mode (so keystrokes go through, not line-
//     buffered) when Stdin is a TTY. Restored on return.
//   - SIGWINCH propagation: when the local terminal resizes, send
//     a "window-change" SSH request to update the remote PTY size.
//
// Exit code mapping mirrors Exec: the remote's exit status when one
// was sent; -1 for signal-only exits or transport errors.
func (c *Client) Shell(ctx context.Context, opts ShellOptions) (int, error) {
	if c == nil || c.ssh == nil {
		return -1, errors.New("sshclient: nil Client")
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.TermType == "" {
		opts.TermType = "xterm-256color"
	}

	sess, err := c.ssh.NewSession()
	if err != nil {
		return -1, fmt.Errorf("open session: %w", err)
	}
	defer sess.Close()

	// Detect local TTY for raw-mode + SIGWINCH wiring. Non-TTY stdin
	// is still legal (piped input); we just skip terminal-mode setup.
	stdinFile, _ := opts.Stdin.(*os.File)
	isTTY := stdinFile != nil && term.IsTerminal(int(stdinFile.Fd()))

	width, height := opts.Width, opts.Height
	if isTTY && (width == 0 || height == 0) {
		if w, h, err := term.GetSize(int(stdinFile.Fd())); err == nil {
			width, height = w, h
		}
	}
	if width == 0 {
		width = 80
	}
	if height == 0 {
		height = 24
	}

	if err := sess.RequestPty(opts.TermType, height, width, ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}); err != nil {
		return -1, fmt.Errorf("pty-req: %w", err)
	}

	sess.Stdin = opts.Stdin
	sess.Stdout = opts.Stdout
	sess.Stderr = opts.Stderr

	var restoreTerm func()
	if isTTY {
		oldState, err := term.MakeRaw(int(stdinFile.Fd()))
		if err == nil {
			restoreTerm = func() { _ = term.Restore(int(stdinFile.Fd()), oldState) }
			defer restoreTerm()
			// Local terminal is now in raw mode (no kernel ONLCR), and
			// the remote outpost runs an in-process emulated PTY that
			// ignores RFC 4254 termios opcodes — so the remote can't be
			// told to do OPOST+ONLCR either. Translate bare \n -> \r\n
			// on the wire between the SSH channel and the local
			// stdout/stderr so command output lands at column 0 instead
			// of staircasing under the prompt column. sh/interactive
			// already writes a single \r after readline to fix the
			// Enter-key column drop; this handles every other \n the
			// remote shell or its children emit during a command.
			sess.Stdout = &crlfTranslator{w: opts.Stdout}
			sess.Stderr = &crlfTranslator{w: opts.Stderr}
		}
	}

	// Propagate SIGWINCH to the remote PTY for as long as the session
	// runs. Closed via the deferred cancel.
	winch, winchCancel := setupSIGWINCH(stdinFile, sess, isTTY)
	defer winchCancel()
	_ = winch

	// Close the session if the parent context fires (Ctrl-C from the
	// outer wrapper, deadline, etc.).
	doneCtx := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Close()
		case <-doneCtx:
		}
	}()

	runErr := sess.Shell()
	if runErr != nil {
		close(doneCtx)
		return -1, fmt.Errorf("start shell: %w", runErr)
	}
	runErr = sess.Wait()
	close(doneCtx)

	exit := 0
	if runErr != nil {
		var exitErr *ssh.ExitError
		if errors.As(runErr, &exitErr) {
			exit = exitErr.ExitStatus()
		} else {
			exit = -1
		}
	}
	return exit, nil
}

// setupSIGWINCH wires the local SIGWINCH to a "window-change" SSH
// request that resizes the remote PTY. Returns the signal channel and
// a cancel func that stops the goroutine.
func setupSIGWINCH(stdinFile *os.File, sess *ssh.Session, isTTY bool) (chan os.Signal, func()) {
	winch := make(chan os.Signal, 1)
	stop := make(chan struct{})
	cancel := func() {
		signal.Stop(winch)
		close(stop)
	}
	if !isTTY || stdinFile == nil {
		return winch, cancel
	}
	signal.Notify(winch, sigwinch)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-winch:
				w, h, err := term.GetSize(int(stdinFile.Fd()))
				if err != nil {
					continue
				}
				// RFC 4254 §6.7: window-change payload is
				// (width, height, pixWidth, pixHeight), each big-endian
				// uint32. Pixels are advisory; 0 is standard.
				payload := []byte{
					byte(w >> 24), byte(w >> 16), byte(w >> 8), byte(w),
					byte(h >> 24), byte(h >> 16), byte(h >> 8), byte(h),
					0, 0, 0, 0,
					0, 0, 0, 0,
				}
				_, _ = sess.SendRequest("window-change", false, payload)
			}
		}
	}()
	return winch, cancel
}

// LocalForward opens a local listener and bridges every accepted
// connection to (destHost, destPort) on the remote outpost's reachable
// network via direct-tcpip. Blocks until ctx is canceled or the
// listener errors. The CLI tunnel subcommand owns lifecycle.
//
// The listener is closed when LocalForward returns.
func (c *Client) LocalForward(ctx context.Context, listener net.Listener, destHost string, destPort int) error {
	if c == nil || c.ssh == nil {
		return errors.New("sshclient: nil Client")
	}
	defer listener.Close()
	if destPort <= 0 {
		return fmt.Errorf("sshclient: invalid destPort %d", destPort)
	}

	// Close the listener on ctx so Accept() unblocks.
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	var wg sync.WaitGroup
	for {
		conn, err := listener.Accept()
		if err != nil {
			wg.Wait()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		wg.Add(1)
		go func(local net.Conn) {
			defer wg.Done()
			defer local.Close()
			remote, derr := c.DirectTCPIP(ctx, destHost, destPort)
			if derr != nil {
				// Drop the local conn; the operator-visible failure
				// is reported by the next LocalForward error or by
				// the client side observing closed conn.
				return
			}
			defer remote.Close()
			// Watch ctx so a tunnel-down signal unblocks the io.Copy
			// halves by force-closing both conns. Without this, an
			// idle tunneled session (no bytes flowing either way)
			// would hold the LocalForward goroutine open past the
			// caller's intent to shut down.
			watchDone := make(chan struct{})
			go func() {
				select {
				case <-ctx.Done():
					_ = local.Close()
					_ = remote.Close()
				case <-watchDone:
				}
			}()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
			go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
			<-done
			close(watchDone)
		}(conn)
	}
}

// SFTP opens an SFTP subsystem channel and wraps it with pkg/sftp's
// client. Caller owns the returned *sftp.Client — call Close before
// closing the outer Client.
func (c *Client) SFTP() (*sftp.Client, error) {
	if c == nil || c.ssh == nil {
		return nil, errors.New("sshclient: nil Client")
	}
	return sftp.NewClient(c.ssh)
}

// crlfTranslator wraps an io.Writer and rewrites bare LF to CRLF. Used
// by Shell() when the local terminal is in raw mode: the remote outpost
// runs an in-process emulated PTY that ignores RFC 4254 termios opcodes
// (see internal/agent/ssh.go:1131 — Modelist is intentionally
// discarded), so OPOST+ONLCR can't be set server-side, and the local
// raw terminal performs no LF→CRLF translation either. The remote
// shell's bare \n would therefore land mid-row at the prompt column,
// staircasing every command's output. Translating on the client side
// is the surgical equivalent of what kernel-PTY OPOST+ONLCR would do.
//
// Already-CRLF runs pass through unchanged (lastR suppresses the second
// translation when an explicit \r precedes \n). Stray \r alone is
// passed through too, since apps emit it deliberately to redraw a line.
type crlfTranslator struct {
	w     io.Writer
	lastR bool
}

func (c *crlfTranslator) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	buf := make([]byte, 0, len(p)+8)
	lastR := c.lastR
	for _, b := range p {
		if b == '\n' && !lastR {
			buf = append(buf, '\r', '\n')
		} else {
			buf = append(buf, b)
		}
		lastR = b == '\r'
	}
	if _, err := c.w.Write(buf); err != nil {
		return 0, err
	}
	c.lastR = lastR
	return len(p), nil
}
