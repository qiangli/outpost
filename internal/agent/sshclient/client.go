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
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
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
		// No AuthMethods. The remote /ssh server flips NoClientAuth=true
		// when cloudbox stamps X-Periscope-Role on the WS upgrade — see
		// internal/agent/ssh.go's PasswordCallback branch. The matrix
		// tunnel + elev cookie is the auth boundary.
		Auth:            nil,
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
