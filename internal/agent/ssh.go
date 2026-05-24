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
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/outpost/internal/agent/peerhosts"
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
func sshHandler(hostKey ssh.Signer, auth hostauth.Authenticator, authURL string, allowLocalForward bool, allowRemoteForward bool, allowAgentForward bool, sftpEnabled bool, peers *peerhosts.Registry) gin.HandlerFunc {
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

		// Route global requests: `tcpip-forward` / `cancel-tcpip-forward`
		// (the `ssh -R` mechanism) get real handlers; everything else
		// (keepalive, no-more-sessions@openssh.com, …) is rejected or
		// silently consumed in the default branch.
		fwds := newForwardRegistry()
		defer fwds.closeAll()
		go func() {
			for req := range reqs {
				switch req.Type {
				case "tcpip-forward":
					handleTCPIPForward(ctx, serverConn, fwds, allowRemoteForward, req)
				case "cancel-tcpip-forward":
					handleCancelTCPIPForward(fwds, req)
				default:
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
				}
			}
		}()

		for newCh := range chans {
			switch newCh.ChannelType() {
			case "session":
				ch, chReqs, aerr := newCh.Accept()
				if aerr != nil {
					slog.Warn("ssh channel accept", "err", aerr)
					continue
				}
				go handleSSHSession(ctx, serverConn, ch, chReqs, sftpEnabled, allowAgentForward)
			case "direct-tcpip":
				if !allowLocalForward {
					_ = newCh.Reject(ssh.Prohibited,
						"local port forwarding disabled by agent config")
					continue
				}
				go handleDirectTCPIP(ctx, newCh, peers)
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
// Loopback is always allowed — it matches the `AppConfig{host:
// 127.0.0.1}` posture of TCP-mode apps and covers the common workflow
// (operator's `ssh -L 7778:localhost:7777 host` against a service the
// agent itself could already reach via a session-channel
// `nc localhost 7777`).
//
// When `peers` is non-nil, any hostname registered as a paired outpost
// in this cloudbox account is also allowed — that enables
// `ssh -J novicortex novidesign`, since `novidesign` is itself a
// reachable outpost. The trust delegation is bounded:
//   - The inner SSH handshake still goes through the destination
//     outpost's own OS-password gate (or matrix_elev cookie), so
//     `peers` membership alone never grants shell access.
//   - Cloudbox is the source of truth for "which hosts share this
//     account"; the registry just caches what it already returns to
//     `outpost ssh-config`.
//
// Anything outside loopback or peers is rejected to keep the agent
// from being repurposed as a generic SOCKS/HTTP proxy into the agent's
// LAN.
func allowDirectTCPIPDest(ctx context.Context, host string, peers *peerhosts.Registry) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	switch h {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	if peers != nil && peers.IsPeer(ctx, h) {
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
func handleDirectTCPIP(ctx context.Context, newCh ssh.NewChannel, peers *peerhosts.Registry) {
	var msg directTCPIPMsg
	if err := ssh.Unmarshal(newCh.ExtraData(), &msg); err != nil {
		slog.Warn("direct-tcpip: bad payload", "err", err)
		_ = newCh.Reject(ssh.ConnectionFailed, "malformed direct-tcpip payload")
		return
	}
	if !allowDirectTCPIPDest(ctx, msg.HostToConnect, peers) {
		slog.Info("direct-tcpip: refused destination not in allowlist",
			"host", msg.HostToConnect, "port", msg.PortToConnect)
		_ = newCh.Reject(ssh.Prohibited,
			"destination not in allowlist (loopback or paired-outpost only; host="+msg.HostToConnect+")")
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

// tcpipForwardMsg is the wire format of an SSH "tcpip-forward" global
// request payload (RFC 4254 §7.1) — the `ssh -R` primitive. The same
// shape is reused for "cancel-tcpip-forward".
type tcpipForwardMsg struct {
	BindAddr string
	BindPort uint32
}

// tcpipForwardReplyMsg is the success reply payload when the client asked
// for BindPort == 0 (let the server pick) — we tell it which port we
// actually bound. RFC 4254 §7.1.
type tcpipForwardReplyMsg struct {
	BoundPort uint32
}

// forwardedTCPIPMsg is the channel-open payload the agent sends when a
// `tcpip-forward` listener accepts a connection and we push it back to
// the client as a `forwarded-tcpip` channel. RFC 4254 §7.2.
type forwardedTCPIPMsg struct {
	DestAddr string
	DestPort uint32
	OrigAddr string
	OrigPort uint32
}

// allowTCPIPForwardBind restricts which bind addresses the agent will
// accept for a `tcpip-forward` listener. Loopback only, mirroring the
// `allowDirectTCPIPDest` posture for `direct-tcpip`. Empty BindAddr is
// treated as "127.0.0.1" — that's what openssh's sshd does too, and a
// 0.0.0.0 bind on the home host would expose the operator's laptop to
// the agent's LAN, which is outside the trust model.
func allowTCPIPForwardBind(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	switch h {
	case "", "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// canonicalBindAddr maps the loopback-equivalent inputs to a single
// representation so listener-registry keys are stable across a
// `tcpip-forward` / `cancel-tcpip-forward` pair that uses different
// spellings.
func canonicalBindAddr(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" || h == "localhost" {
		return "127.0.0.1"
	}
	return h
}

// forwardRegistry tracks listeners spawned by `tcpip-forward` requests on
// one SSH connection. Keyed by "addr:port" of the bound listener. Lifetime
// is per-SSH-conn — the dispatcher's deferred closeAll() in sshHandler
// covers serverConn teardown.
type forwardRegistry struct {
	mu  sync.Mutex
	lns map[string]net.Listener
}

func newForwardRegistry() *forwardRegistry {
	return &forwardRegistry{lns: make(map[string]net.Listener)}
}

func (r *forwardRegistry) add(key string, ln net.Listener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lns[key] = ln
}

func (r *forwardRegistry) remove(key string) net.Listener {
	r.mu.Lock()
	defer r.mu.Unlock()
	ln := r.lns[key]
	delete(r.lns, key)
	return ln
}

func (r *forwardRegistry) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, ln := range r.lns {
		_ = ln.Close()
		delete(r.lns, k)
	}
}

func fwdKey(addr string, port uint32) string {
	return net.JoinHostPort(addr, strconv.Itoa(int(port)))
}

// handleTCPIPForward services one `tcpip-forward` global request: bind a
// loopback listener at the requested port (or one picked by the OS when
// BindPort == 0), register it, and run an accept loop that pushes each
// accepted connection back to the SSH client as a `forwarded-tcpip`
// channel.
//
// Trust model: same OS-password gate that already protects session and
// direct-tcpip channels. The bind is loopback-only by policy regardless
// of `allowRemoteForward`, so widening reach to the LAN is impossible
// from this surface.
func handleTCPIPForward(ctx context.Context, sc *ssh.ServerConn, fwds *forwardRegistry, allowRemoteForward bool, req *ssh.Request) {
	if !allowRemoteForward {
		_ = req.Reply(false, nil)
		return
	}
	var msg tcpipForwardMsg
	if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
		slog.Warn("tcpip-forward: bad payload", "err", err)
		_ = req.Reply(false, nil)
		return
	}
	if !allowTCPIPForwardBind(msg.BindAddr) {
		slog.Info("tcpip-forward: refused non-loopback bind",
			"bind_addr", msg.BindAddr, "bind_port", msg.BindPort)
		_ = req.Reply(false, nil)
		return
	}
	bindAddr := canonicalBindAddr(msg.BindAddr)

	ln, err := net.Listen("tcp", net.JoinHostPort(bindAddr, strconv.Itoa(int(msg.BindPort))))
	if err != nil {
		slog.Info("tcpip-forward: listen failed", "bind", bindAddr, "port", msg.BindPort, "err", err)
		_ = req.Reply(false, nil)
		return
	}
	boundPort := uint32(ln.Addr().(*net.TCPAddr).Port)
	key := fwdKey(bindAddr, boundPort)
	fwds.add(key, ln)

	// RFC 4254 §7.1: reply payload carries the bound port only when the
	// client asked the server to pick (BindPort == 0). Replying with no
	// payload when BindPort != 0 keeps openssh-clients happy.
	if msg.BindPort == 0 {
		_ = req.Reply(true, ssh.Marshal(tcpipForwardReplyMsg{BoundPort: boundPort}))
	} else {
		_ = req.Reply(true, nil)
	}
	slog.Info("tcpip-forward: listening", "bind", bindAddr, "port", boundPort)

	// clientDestAddr / clientDestPort are what we'll stuff into the
	// `forwarded-tcpip` channel-open payload when a connection arrives.
	// They must match what the client recorded at tcpip-forward time —
	// OpenSSH's client looks up its forward table with a `strcmp` on
	// listen_address and `==` on listen_port. So we echo the ORIGINAL
	// bind_addr the client sent (NOT canonicalBindAddr — that would
	// turn "" into "127.0.0.1" and the lookup would fail with the
	// `WARNING: Server requests forwarding for unknown listen_port`
	// noise that breaks the channel). For BindPort, the only time
	// boundPort can differ from msg.BindPort is the ephemeral-port
	// case (BindPort == 0) — in which case the client recorded the
	// port we echoed back via tcpipForwardReplyMsg, so we send that.
	clientDestAddr := msg.BindAddr
	clientDestPort := msg.BindPort
	if clientDestPort == 0 {
		clientDestPort = boundPort
	}

	go func() {
		defer func() {
			_ = ln.Close()
			// Remove from registry whether we got here via cancel
			// (registry already empty for this key) or via accept-loop
			// error — keeps the map from leaking on accept failures.
			fwds.remove(key)
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go bridgeForwardedTCPIP(ctx, sc, c, clientDestAddr, clientDestPort)
		}
	}()
}

// handleCancelTCPIPForward services `cancel-tcpip-forward` by closing the
// matching listener (which trips its accept loop's exit path).
func handleCancelTCPIPForward(fwds *forwardRegistry, req *ssh.Request) {
	var msg tcpipForwardMsg
	if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
		_ = req.Reply(false, nil)
		return
	}
	bindAddr := canonicalBindAddr(msg.BindAddr)
	key := fwdKey(bindAddr, msg.BindPort)
	if ln := fwds.remove(key); ln != nil {
		_ = ln.Close()
		_ = req.Reply(true, nil)
		return
	}
	_ = req.Reply(false, nil)
}

// bridgeForwardedTCPIP opens a `forwarded-tcpip` channel back to the SSH
// client and byte-bridges it to the accepted local connection. Mirror
// image of handleDirectTCPIP — the io.Copy pattern is identical.
//
// destAddr/destPort MUST be the original values the client requested in
// its tcpip-forward — the client uses them as the lookup key into its
// remote-forward table (`strcmp` on address, `==` on port). See
// handleTCPIPForward for the reasoning.
func bridgeForwardedTCPIP(ctx context.Context, sc *ssh.ServerConn, c net.Conn, destAddr string, destPort uint32) {
	_ = ctx // matches handleDirectTCPIP's signature; the io.Copy pair drives teardown
	defer c.Close()
	origHost, origPortStr, _ := net.SplitHostPort(c.RemoteAddr().String())
	origPort, _ := strconv.Atoi(origPortStr)
	payload := ssh.Marshal(forwardedTCPIPMsg{
		DestAddr: destAddr,
		DestPort: destPort,
		OrigAddr: origHost,
		OrigPort: uint32(origPort),
	})
	ch, chReqs, err := sc.OpenChannel("forwarded-tcpip", payload)
	if err != nil {
		slog.Info("forwarded-tcpip: channel open rejected",
			"dest", destAddr, "port", destPort, "err", err)
		return
	}
	defer ch.Close()
	// `forwarded-tcpip` channels never carry channel requests — drain to
	// keep the crypto/ssh request goroutine from piling up.
	go ssh.DiscardRequests(chReqs)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(c, ch)
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ch, c)
		_ = ch.CloseWrite()
	}()
	wg.Wait()
}

// ptyReqMsg is the wire format of an SSH "pty-req" channel request payload
// (RFC 4254 §6.2). We consume Term + Columns/Rows; Modelist (termios
// opcodes per RFC 4254 §8 — ECHO, ISIG, ICRNL, …) is intentionally
// ignored because Linux/macOS PTYs come up with sensible defaults and the
// /shell xterm.js path has shipped without them just fine. WidthPx/HeightPx
// only matter for graphical-cell-aware terminals, which we don't support.
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

// subsystemReqMsg is the wire format of an SSH "subsystem" channel request
// payload (RFC 4254 §6.5: a single string naming the subsystem).
type subsystemReqMsg struct {
	Name string
}

// exitStatusMsg is the wire format of an SSH "exit-status" channel request
// payload (RFC 4254 §6.10).
type exitStatusMsg struct {
	Status uint32
}

// handleSSHSession handles one "session" channel — its lifecycle is the
// stream of channel requests (pty-req, window-change, env, shell, exec,
// subsystem) terminated by the channel close.
func handleSSHSession(ctx context.Context, sc *ssh.ServerConn, ch ssh.Channel, reqs <-chan *ssh.Request, sftpEnabled bool, allowAgentForward bool) {
	defer ch.Close()

	var (
		ptyTerm string
		ptyCols uint16
		ptyRows uint16
		session *outshell.Session
		af      *agentForward
	)
	defer func() {
		if session != nil {
			_ = session.Close()
		}
		af.Close() // nil-safe
	}()

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			var msg ptyReqMsg
			if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			ptyTerm = msg.Term
			ptyCols = uint16(msg.Columns)
			ptyRows = uint16(msg.Rows)
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

		case "auth-agent-req@openssh.com":
			// `ssh -A` — agent forwarding. Set up the per-session Unix
			// socket BEFORE replying so SSH_AUTH_SOCK is ready by the
			// time the client sends `shell` / `exec`. The socket lives
			// in a 0700 tempdir and the channel-back into the client is
			// opened lazily on each accept(socket) — see sshagent.go.
			if !allowAgentForward || af != nil {
				_ = req.Reply(false, nil)
				continue
			}
			a, err := startAgentForward(ctx, sc)
			if err != nil {
				slog.Warn("auth-agent-req: setup failed", "err", err)
				_ = req.Reply(false, nil)
				continue
			}
			af = a
			_ = req.Reply(true, nil)

		case "shell":
			if session != nil {
				_ = req.Reply(false, nil)
				continue
			}
			s, err := outshell.NewSession(outshell.SessionOptions{
				Term: ptyTerm,
				Cols: ptyCols,
				Rows: ptyRows,
				Env:  agentForwardEnv(af),
			})
			if err != nil {
				slog.Error("ssh shell session", "err", err)
				_ = req.Reply(false, nil)
				return
			}
			session = s
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
			var status uint32
			if ptyTerm != "" {
				// Client asked for a TTY before exec (`ssh -tt host cmd`).
				// Allocate a PTY-backed session so `tty`, `screen -dmS`,
				// curses tools, etc. see a real /dev/ttysNN — without this
				// they fall back to "not a tty" and many tools refuse to
				// run at all.
				s, err := outshell.NewSession(outshell.SessionOptions{
					Term: ptyTerm,
					Cols: ptyCols,
					Rows: ptyRows,
					Env:  agentForwardEnv(af),
				})
				if err != nil {
					slog.Error("ssh exec session", "err", err)
					status = 1
				} else {
					session = s
					status = runExecCommandPTY(ctx, ch, session, msg.Command)
				}
			} else {
				status = runExecCommand(ctx, ch, msg.Command, agentForwardEnv(af))
			}
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(exitStatusMsg{Status: status}))
			return

		case "subsystem":
			var msg subsystemReqMsg
			if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			if msg.Name != "sftp" || !sftpEnabled {
				// netconf / anything else / sftp-when-disabled. Reject;
				// well-behaved clients fall back to exec.
				_ = req.Reply(false, nil)
				continue
			}
			if session != nil {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			serveSFTP(ctx, ch)
			return

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

// runExecCommandPTY runs a one-shot command (the SSH "exec" request)
// through a PTY-backed Session — the path taken when the client did a
// `pty-req` first (`ssh -tt host cmd`). `tty`, `screen -dmS`, and
// curses tools see a real /dev/ttysNN here, which they need.
//
// The shape of the pipe loops mirrors runInteractiveShell: PTY master
// ↔ SSH channel, the runner runs in its own goroutine, and we wait for
// whichever side finishes first.
func runExecCommandPTY(ctx context.Context, ch ssh.Channel, session *outshell.Session, command string) uint32 {
	// outputDone closes when the PTY→channel goroutine has drained
	// everything the kernel had buffered for us — closing the master
	// before then would lose in-flight bytes (which is exactly the
	// "ssh -tt host tty produces no output" symptom).
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		_, _ = io.Copy(ch, session.Master())
	}()
	// channel → PTY (the command's stdin). We DON'T treat EOF here as
	// "client disconnected" — openssh closes its outgoing stdin
	// immediately after sending the exec request when there's nothing
	// to pipe in (`ssh host cmd </dev/null`-style), and any earlier
	// version that did made the exec exit before it could even run.
	// If the client really did close the whole channel, the runner's
	// next Write blows up and the runner exits on its own.
	go func() {
		_, _ = io.Copy(session.Master(), ch)
	}()
	statusCh := make(chan uint32, 1)
	go func() {
		statusCh <- session.RunOnce(ctx, command)
	}()

	var status uint32
	select {
	case status = <-statusCh:
		// Command finished. Close the SLAVE side first — that signals
		// EOF without dropping data sitting in the kernel buffer. The
		// output goroutine then drains and exits on EOF; only then is
		// it safe to close the master.
		_ = session.CloseSlave()
		<-outputDone
	case <-ctx.Done():
		status = 130
	}
	_ = session.Close()
	return status
}

// agentForwardEnv returns the env-overrides map containing
// SSH_AUTH_SOCK when agent forwarding is active, or nil when not. Used
// by both the shell and exec code paths so the runner sees the
// per-session socket without needing a custom env builder.
func agentForwardEnv(af *agentForward) map[string]string {
	if af == nil {
		return nil
	}
	return map[string]string{"SSH_AUTH_SOCK": af.SocketPath()}
}

// runExecCommand executes a one-shot shell command (the SSH "exec"
// request: `ssh host -- cmd`) through the qiangli/sh interpreter without
// a PTY. Stdout and stderr are merged onto the channel — same convention
// as openssh's default exec mode without -t.
//
// Used by scp and rsync (which both invoke the remote side via exec).
// envOverrides carries SSH_AUTH_SOCK when `ssh -A` is in effect; nil
// otherwise.
func runExecCommand(ctx context.Context, ch ssh.Channel, command string, envOverrides map[string]string) uint32 {
	parser := syntax.NewParser()
	file, err := parser.Parse(strings.NewReader(command), "")
	if err != nil {
		_, _ = io.WriteString(ch.Stderr(), err.Error()+"\n")
		return 127
	}
	runner, err := interp.New(
		interp.StdIO(ch, ch, ch.Stderr()),
		interp.Env(outshell.BuildEnvWith(envOverrides)),
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

// serveSFTP runs an SFTP server over the SSH channel. Filesystem access
// runs in the outpost process's OS-user context — the /auth password gate
// has already proved the caller knows that user's password, so the
// trust model matches the /shell and /ssh interactive paths (any read or
// write the OS user can do, the SFTP client can do). The pkg/sftp server
// does its own context handling and exits cleanly when the channel closes.
func serveSFTP(ctx context.Context, ch ssh.Channel) {
	defer ch.Close()
	srv, err := sftp.NewServer(ch)
	if err != nil {
		slog.Warn("sftp server init", "err", err)
		return
	}
	// pkg/sftp's Server.Serve blocks until the underlying conn returns
	// EOF (client closed) or an error. Hook ctx cancellation to close
	// the channel from underneath it so a server shutdown actually tears
	// the SFTP session down.
	stop := context.AfterFunc(ctx, func() {
		_ = srv.Close()
	})
	defer stop()
	status := uint32(0)
	if err := srv.Serve(); err != nil && !errors.Is(err, io.EOF) {
		slog.Info("sftp server exit", "err", err)
		status = 1
	}
	// openssh scp escalates the absence of an exit-status reply on the
	// SSH channel to "Exit status -1" / non-zero scp exit, which then
	// cascades to scripts. Send an explicit 0 (or 1) so scp's success
	// path lines up with the data path.
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(exitStatusMsg{Status: status}))
}
