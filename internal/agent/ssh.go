package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/qiangli/outpost/internal/agent/hostauth"
	outshell "github.com/qiangli/outpost/internal/agent/shell"
)

// sshHandler is the agent's GET /ssh WebSocket endpoint. Cloudbox proxies
// raw bytes through the matrix tunnel; this handler wraps the WS as a
// net.Conn and hands it to an in-process golang.org/x/crypto/ssh server.
//
// Trust model (mirrors /shell and /desktop):
//   - Cloudbox authenticates the OAuth user and decides whether the host
//     is visible to the caller. It is a pure transport for the SSH bytes.
//   - The OS password is the gate. The SSH PasswordCallback delegates to
//     the same hostauth.Authenticator that /auth uses — PAM on Linux,
//     dscl on macOS, LogonUserW on Windows.
//   - The submitted SSH username MUST equal the agent's running OS user
//     (same constraint as /auth). Anything else is rejected before we
//     touch PAM, so we never silently weaken the gate.
func sshHandler(hostKey ssh.Signer, auth hostauth.Authenticator, authURL string, allowLocalForward bool) gin.HandlerFunc {
	currentUser, _ := hostauth.CurrentUser()
	authURL = strings.TrimSpace(authURL)

	return func(c *gin.Context) {
		// Cloudbox stamps X-Periscope-Role on the WSS upgrade after its
		// own elevation gate passes. When present and >= "user", the
		// caller has already been authenticated at the cloudbox edge
		// (matrix_elev cookie minted by /h/:host/elevate against the
		// outpost's /auth PAM check). We honor that vouching and skip
		// the SSH-protocol password challenge — otherwise the user
		// would be prompted for the OS password twice on every session.
		//
		// Loopback-only binding + matrix-tunnel ingress make this
		// header trustworthy (same model /shell already trusts
		// X-Periscope-User on). Direct-loopback access bypassing
		// cloudbox falls through to the password fallback below.
		periscopeRole := strings.TrimSpace(c.GetHeader("X-Periscope-Role"))
		cloudboxVouched := periscopeRole == "admin" || periscopeRole == "user"

		// Loopback-only, reached only through the cloud's WS proxy. Same
		// origin-skip rationale as shellHandler.
		ws, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			slog.Warn("ssh ws accept", "err", err)
			return
		}
		defer ws.Close(websocket.StatusInternalError, "closing")

		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()

		// Treat the WS as a TCP-like byte stream. Both sides use binary
		// frames; the framing is transparent to the SSH protocol.
		netConn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
		defer netConn.Close()

		serverConfig := &ssh.ServerConfig{
			MaxAuthTries: 3,
			// When cloudbox already vouched (matrix_elev gate passed),
			// flip to NoClientAuth so the SSH handshake completes
			// without a password challenge — fully unattended for
			// agentic tools that ride on a previously-cached cookie.
			NoClientAuth: cloudboxVouched,
			PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
				user := strings.TrimSpace(meta.User())

				// AuthURL path: delegate fully. The endpoint owns its
				// user list and the role decision; we just accept/reject.
				if authURL != "" {
					if _, _, derr := delegateAuth(authURL, AuthRequest{User: user, Password: string(password)}, ""); derr != nil {
						return nil, fmt.Errorf("invalid credentials")
					}
					return nil, nil
				}

				// OS path: username must equal the running OS user.
				if currentUser == "" {
					return nil, fmt.Errorf("cannot determine current user")
				}
				if !strings.EqualFold(user, currentUser) {
					return nil, fmt.Errorf("invalid credentials")
				}
				if err := auth.Authenticate(currentUser, string(password)); err != nil {
					return nil, fmt.Errorf("invalid credentials")
				}
				return nil, nil
			},
		}
		serverConfig.AddHostKey(hostKey)

		serverConn, chans, reqs, err := ssh.NewServerConn(netConn, serverConfig)
		if err != nil {
			slog.Info("ssh handshake failed", "err", err, "remote", c.Request.RemoteAddr)
			return
		}
		defer serverConn.Close()

		// Discard out-of-channel global requests (keepalive,
		// tcpip-forward, no-more-sessions@openssh.com, etc.). The
		// keepalive pings clients still rely on; `tcpip-forward` (used
		// by `ssh -R` reverse forwards) we intentionally don't honor
		// yet — adding direct-tcpip below covers the common `ssh -L`
		// operator workflow without the lifecycle complexity of
		// agent-side listeners.
		go ssh.DiscardRequests(reqs)

		for newCh := range chans {
			switch newCh.ChannelType() {
			case "session":
				ch, chReqs, aerr := newCh.Accept()
				if aerr != nil {
					slog.Warn("ssh channel accept", "err", aerr)
					continue
				}
				go handleSSHSession(ctx, ch, chReqs)
			case "direct-tcpip":
				if !allowLocalForward {
					_ = newCh.Reject(ssh.Prohibited,
						"local port forwarding disabled by agent config")
					continue
				}
				go handleDirectTCPIP(ctx, newCh)
			default:
				_ = newCh.Reject(ssh.UnknownChannelType,
					"only session and direct-tcpip channels are supported")
			}
		}
	}
}

// directTCPIPMsg is the channel-data payload SSH clients send when
// opening a `direct-tcpip` channel — the primitive behind `ssh -L`
// local port forwards and `ssh -D` SOCKS (RFC 4254 §7.2).
type directTCPIPMsg struct {
	HostToConnect       string
	PortToConnect       uint32
	OriginatorIPAddress string
	OriginatorPort      uint32
}

// allowDirectTCPIPDest restricts which destinations a paired agent will
// dial on behalf of an authenticated SSH client.
//
// Loopback-only is the safe default and matches the `AppConfig{host:
// 127.0.0.1}` posture used by the existing TCP-mode app mechanism: it
// covers the common workflow (operator's `ssh -L 7778:localhost:7777
// host` against a service the agent itself could already reach via a
// session-channel `nc localhost 7777`), and it stops the agent from
// being repurposed as a generic SOCKS/HTTP proxy into the agent
// machine's LAN. Widening to an allowlisted set of host:port targets
// is a separate config-surface change tracked alongside `ssh -R`
// support.
func allowDirectTCPIPDest(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	switch h {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// handleDirectTCPIP services one `direct-tcpip` channel-open request:
// parse the payload, dial the requested loopback target, and
// bidirectionally bridge bytes between the SSH channel and the local
// connection. Caller is the channel-dispatch loop in sshHandler —
// always invoked in its own goroutine so multiple forwards multiplex
// freely inside the same SSH connection.
//
// Trust model: same OS-password gate that already protects session
// channels in this server. Anyone with shell access here today can
// `ssh ... 'nc 127.0.0.1 7777'` via a session channel, so accepting
// the multiplexed direct-tcpip form adds no authority.
func handleDirectTCPIP(ctx context.Context, newCh ssh.NewChannel) {
	var msg directTCPIPMsg
	if err := ssh.Unmarshal(newCh.ExtraData(), &msg); err != nil {
		slog.Warn("direct-tcpip: bad payload", "err", err)
		_ = newCh.Reject(ssh.ConnectionFailed, "malformed direct-tcpip payload")
		return
	}
	if !allowDirectTCPIPDest(msg.HostToConnect) {
		slog.Info("direct-tcpip: refused non-loopback target",
			"host", msg.HostToConnect, "port", msg.PortToConnect)
		_ = newCh.Reject(ssh.Prohibited,
			"only loopback destinations are allowed (host="+msg.HostToConnect+")")
		return
	}
	target := net.JoinHostPort(msg.HostToConnect, strconv.Itoa(int(msg.PortToConnect)))

	d := net.Dialer{}
	upstream, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		slog.Info("direct-tcpip: dial failed", "target", target, "err", err)
		_ = newCh.Reject(ssh.ConnectionFailed, "dial "+target+": "+err.Error())
		return
	}
	defer upstream.Close()

	ch, chReqs, aerr := newCh.Accept()
	if aerr != nil {
		slog.Warn("direct-tcpip: channel accept", "target", target, "err", aerr)
		return
	}
	defer ch.Close()
	// `direct-tcpip` channels never carry channel requests — drain
	// any spurious ones so the crypto/ssh request goroutine doesn't
	// pile up against an unread channel.
	go ssh.DiscardRequests(chReqs)

	slog.Info("direct-tcpip: bridging", "target", target,
		"origin", msg.OriginatorIPAddress+":"+strconv.Itoa(int(msg.OriginatorPort)))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, ch)
		if tc, ok := upstream.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ch, upstream)
		_ = ch.CloseWrite()
	}()
	wg.Wait()
}

// ptyReqMsg is the wire format of an SSH "pty-req" channel request payload
// (RFC 4254 §6.2). We only consume Columns/Rows.
type ptyReqMsg struct {
	Term     string
	Columns  uint32
	Rows     uint32
	WidthPx  uint32
	HeightPx uint32
	Modelist string
}

// windowChangeMsg is the wire format of an SSH "window-change" channel
// request payload (RFC 4254 §6.7).
type windowChangeMsg struct {
	Columns  uint32
	Rows     uint32
	WidthPx  uint32
	HeightPx uint32
}

// execMsg is the wire format of an SSH "exec" channel request payload
// (RFC 4254 §6.5).
type execMsg struct {
	Command string
}

// exitStatusMsg is the wire format of an SSH "exit-status" channel request
// payload (RFC 4254 §6.10).
type exitStatusMsg struct {
	Status uint32
}

// handleSSHSession handles one "session" channel — its lifecycle is the
// stream of channel requests (pty-req, window-change, env, shell, exec,
// subsystem) terminated by the channel close.
func handleSSHSession(ctx context.Context, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()

	var (
		ptyCols uint16
		ptyRows uint16
		hasPty  bool
		session *outshell.Session
	)
	defer func() {
		if session != nil {
			_ = session.Close()
		}
	}()

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			var msg ptyReqMsg
			if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			ptyCols = uint16(msg.Columns)
			ptyRows = uint16(msg.Rows)
			hasPty = true
			_ = req.Reply(true, nil)

		case "window-change":
			var msg windowChangeMsg
			if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			ptyCols = uint16(msg.Columns)
			ptyRows = uint16(msg.Rows)
			if session != nil {
				_ = session.Resize(ptyCols, ptyRows)
			}
			// window-change has no reply per RFC 4254 §6.7.

		case "env":
			// Ignore env. Allowing arbitrary env-var injection from the
			// client is the openssh-default-deny posture; we follow it.
			_ = req.Reply(true, nil)

		case "shell":
			if session != nil {
				_ = req.Reply(false, nil)
				continue
			}
			s, err := outshell.NewSession()
			if err != nil {
				slog.Error("ssh shell session", "err", err)
				_ = req.Reply(false, nil)
				return
			}
			session = s
			if hasPty {
				_ = session.Resize(ptyCols, ptyRows)
			}
			_ = req.Reply(true, nil)
			runInteractiveShell(ctx, ch, session)
			return

		case "exec":
			if session != nil {
				_ = req.Reply(false, nil)
				continue
			}
			var msg execMsg
			if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			status := runExecCommand(ctx, ch, msg.Command)
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(exitStatusMsg{Status: status}))
			return

		case "subsystem":
			// SFTP / netconf are out of scope for v1. Reject so clients
			// fall back to exec where possible (scp uses exec, not the
			// subsystem channel, so this does not affect rsync/scp).
			_ = req.Reply(false, nil)

		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

// runInteractiveShell wires the qiangli/sh PTY session to the SSH channel
// and blocks until either the runner finishes (e.g. `exit`) or the
// client closes its channel. Both teardown paths converge on closing
// session + channel so neither I/O goroutine is left blocked.
func runInteractiveShell(ctx context.Context, ch ssh.Channel, session *outshell.Session) {
	// PTY master → SSH channel.
	go func() {
		_, _ = io.Copy(ch, session.Master())
	}()
	// SSH channel → PTY master.
	clientGone := make(chan struct{})
	go func() {
		defer close(clientGone)
		_, _ = io.Copy(session.Master(), ch)
	}()
	// Runner; returns when the in-process shell hits its exit builtin
	// or when its PTY slave is closed under it.
	runErr := make(chan error, 1)
	go func() {
		runErr <- session.Run(ctx)
	}()

	select {
	case err := <-runErr:
		if err != nil {
			slog.Info("ssh shell runner exit", "err", err)
		}
	case <-clientGone:
		// Client disconnected first. Closing the session below will
		// trip the runner's next PTY read.
	case <-ctx.Done():
		// Handler context canceled (WS closed, server shutdown).
	}
	_ = session.Close()
	_ = ch.Close()
}

// runExecCommand executes a one-shot shell command (the SSH "exec"
// request: `ssh host -- cmd`) through the qiangli/sh interpreter without
// a PTY. Stdout and stderr are merged onto the channel — same convention
// as openssh's default exec mode without -t.
//
// Used by scp and rsync (which both invoke the remote side via exec).
func runExecCommand(ctx context.Context, ch ssh.Channel, command string) uint32 {
	parser := syntax.NewParser()
	file, err := parser.Parse(strings.NewReader(command), "")
	if err != nil {
		_, _ = io.WriteString(ch.Stderr(), err.Error()+"\n")
		return 127
	}
	runner, err := interp.New(
		interp.StdIO(ch, ch, ch.Stderr()),
		interp.Env(nil),
	)
	if err != nil {
		_, _ = io.WriteString(ch.Stderr(), err.Error()+"\n")
		return 1
	}
	if err := runner.Run(ctx, file); err != nil {
		var ec interp.ExitStatus
		if errors.As(err, &ec) {
			return uint32(ec)
		}
		_, _ = io.WriteString(ch.Stderr(), err.Error()+"\n")
		return 1
	}
	return 0
}
