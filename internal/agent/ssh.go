package agent

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/outpost/internal/agent/peerhosts"
	"github.com/qiangli/outpost/internal/agent/peerticket"
	outshell "github.com/qiangli/outpost/internal/agent/shell"
)

// sshHandlerDeps bundles the per-handler configuration sshHandler needs.
// Using a struct instead of a positional argument list keeps the call
// site (routes.go) readable now that we have nine knobs — host key, auth
// hook, agent-config booleans, plus the streamlocal allowlist.
type sshHandlerDeps struct {
	HostKey            ssh.Signer
	Auth               hostauth.Authenticator
	AuthURL            string
	AllowLocalForward  bool
	AllowRemoteForward bool
	AllowAgentForward  bool
	SFTPEnabled        bool
	Peers              *peerhosts.Registry
	// ForwardSockets is the operator-supplied extension to the built-in
	// unix-socket allowlist for direct-streamlocal@openssh.com. The
	// effective allowlist is built from podman/docker defaults (see
	// defaultStreamlocalAllowlist) plus these paths.
	ForwardSockets []string

	// SelfName is this outpost's AgentName, sent in the
	// X-Outpost-Peer-Origin header on peer-tunneled dials so
	// cloudbox's audit log can record the originating sibling.
	SelfName string

	// TrustPeriscopeRole controls whether the handler accepts the
	// `X-Periscope-Role` header as a vouching signal. True on the
	// loopback-bound handler (where the matrix tunnel is the only
	// ingress, so cloudbox is the only entity that can stamp the
	// header). False on any LAN-bound handler — a LAN listener that
	// honored the header would let any LAN device promote itself to
	// admin by spoofing it. The peer-ticket path (Authorization:
	// Bearer <ticket>, verified locally) is the LAN-side substitute.
	TrustPeriscopeRole bool

	// TicketVerifier and TicketAudience configure the peer-ticket
	// verification path that replaces X-Periscope-Role on LAN-bound
	// handlers. Nil Verifier or empty Pubkey disables verification —
	// the handler falls through to the OS-password gate as if the
	// ticket weren't presented. TicketAudience is the receiver's
	// expected `aud` claim (e.g. "outpost:peer-b"); set from the
	// outpost's own AgentName at boot, never from a request.
	TicketVerifier *peerticket.Verifier
	TicketPubkey   ed25519.PublicKey
	TicketAudience string
	TicketScope    string // capability gate: "ssh" for this handler

	// CloudboxBase is the cloudbox HTTP(S) base URL (e.g.
	// "https://ai.dhnt.io"). When set together with AccessToken, the
	// direct-tcpip handler will tunnel `ssh -J peerA peerB` second-hop
	// dials through cloudbox's /h/<peerB>/ssh WSS endpoint instead of
	// trying a plain net.Dial that would fail on LAN DNS. Empty here
	// means "no cloudbox-tunneled peer dial" — the fallback to net.Dial
	// stays as it was for loopback targets.
	CloudboxBase string

	// CloudboxProtocol mirrors fc.Protocol so the WSS-vs-WS choice for
	// peer dials matches whatever the matrix tunnel uses. Empty value
	// is treated as "ws" (plain — matches cloudboxHTTPBase's defaulting).
	CloudboxProtocol string

	// AccessToken is the per-outpost JWT used as the bearer when this
	// outpost dials cloudbox on behalf of an SSH ProxyJump operator.
	// Must come from the same account that owns the peer registry —
	// that way cloudbox knows the dial is intra-account.
	AccessToken string
}

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
func sshHandler(deps sshHandlerDeps) gin.HandlerFunc {
	currentUser, _ := hostauth.CurrentUser()
	authURL := strings.TrimSpace(deps.AuthURL)
	// Build the effective unix-socket allowlist once at handler init —
	// the defaults are stable for the life of the process and the
	// operator extension only changes on restart (admin-UI edits to
	// FileConfig.SSHForwardSockets trigger a self-restart).
	streamlocalAllow := buildStreamlocalAllowlist(deps.ForwardSockets)

	return func(c *gin.Context) {
		// Two vouching paths feed cloudboxVouched. Both end in the
		// same effect (NoClientAuth=true → no SSH password prompt):
		//
		//  1. X-Periscope-Role header — what cloudbox stamps after
		//     validating the matrix_elev cookie on its own edge. Only
		//     trustworthy when the matrix tunnel is the only ingress
		//     to this handler (loopback bind). The TrustPeriscopeRole
		//     dep gates this: false on LAN-bound handlers because a
		//     LAN listener that honored this header would let any
		//     LAN device promote itself to admin.
		//
		//  2. Authorization: Bearer <peer-ticket> — a short-lived
		//     JWT cloudbox issues at /api/v1/ssh/peer-ticket in
		//     exchange for the client's matrix_elev cookie. The
		//     receiver verifies the signature locally using
		//     CloudboxTicketPubkey (stored at pairing time). Lets the
		//     LAN-direct path stay passwordless without putting
		//     cloudbox in the data plane. The cookie itself never
		//     traverses to the LAN bind — only the derived ticket.
		//
		// Direct-loopback access bypassing both falls through to the
		// PasswordCallback OS-password gate.
		cloudboxVouched := false
		if deps.TrustPeriscopeRole {
			periscopeRole := strings.TrimSpace(c.GetHeader("X-Periscope-Role"))
			cloudboxVouched = periscopeRole == "admin" || periscopeRole == "user"
		}
		if !cloudboxVouched && deps.TicketVerifier != nil && len(deps.TicketPubkey) > 0 {
			if tok := extractBearerTicket(c.Request); tok != "" {
				claims, verr := deps.TicketVerifier.Verify(tok, peerticket.VerifyOptions{
					Pubkey:           deps.TicketPubkey,
					ExpectedAudience: deps.TicketAudience,
					RequiredScope:    deps.TicketScope,
				})
				if verr == nil {
					cloudboxVouched = true
					slog.Info("ssh: peer-ticket accepted",
						"remote", c.Request.RemoteAddr,
						"sub", claims.Subject,
						"role", claims.Role)
				} else {
					slog.Warn("ssh: peer-ticket rejected",
						"remote", c.Request.RemoteAddr,
						"err", verr)
				}
			}
		}

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

		handleSSHConn(ctx, netConn, c.Request.RemoteAddr, cloudboxVouched, deps, currentUser, authURL, streamlocalAllow)
	}
}

// extractBearerTicket returns the peer-ticket from an Authorization
// header of the form `Bearer <ticket>`. Empty string when no header
// is present or the scheme isn't Bearer — callers fall through to
// the password path in that case.
func extractBearerTicket(r *http.Request) string {
	if r == nil {
		return ""
	}
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// handleSSHConn services one already-established byte stream as an
// in-process SSH server. Used by both the WS endpoint (`sshHandler`
// above) and the optional LAN TCP listener (`ServeLANListener` below)
// so the SSH server config + channel dispatch + global-request handling
// stay in one place.
//
// cloudboxVouched controls whether NoClientAuth is enabled. The WS path
// flips it to true when cloudbox stamped X-Periscope-Role; LAN-direct
// always passes false (no upstream vouching available) and the
// PasswordCallback enforces the OS-password gate.
//
// remoteAddr is informational (logging only); pass `""` when not
// available. currentUser, authURL, and streamlocalAllow are hoisted
// to the caller so handler-init work (`hostauth.CurrentUser()`,
// `buildStreamlocalAllowlist(...)`) happens once per handler rather
// than per-connection.
func handleSSHConn(
	ctx context.Context,
	conn net.Conn,
	remoteAddr string,
	cloudboxVouched bool,
	deps sshHandlerDeps,
	currentUser, authURL string,
	streamlocalAllow []string,
) {
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
			if err := deps.Auth.Authenticate(currentUser, string(password)); err != nil {
				return nil, fmt.Errorf("invalid credentials")
			}
			return nil, nil
		},
	}
	serverConfig.AddHostKey(deps.HostKey)

	serverConn, chans, reqs, err := ssh.NewServerConn(conn, serverConfig)
	if err != nil {
		slog.Info("ssh handshake failed", "err", err, "remote", remoteAddr)
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
				handleTCPIPForward(ctx, serverConn, fwds, deps.AllowRemoteForward, req)
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
			go handleSSHSession(ctx, serverConn, ch, chReqs, deps.SFTPEnabled, deps.AllowAgentForward)
		case "direct-tcpip":
			if !deps.AllowLocalForward {
				_ = newCh.Reject(ssh.Prohibited,
					"local port forwarding disabled by agent config")
				continue
			}
			go handleDirectTCPIP(ctx, newCh, deps.Peers, peerDial{
				cloudboxBase:     deps.CloudboxBase,
				cloudboxProtocol: deps.CloudboxProtocol,
				accessToken:      deps.AccessToken,
				selfName:         deps.SelfName,
			})
		case "direct-streamlocal@openssh.com":
			// Podman's `ssh://` transport opens this channel type to
			// forward an SSH channel onto a remote unix socket — i.e.
			// `podman --connection=<host>` against a paired outpost.
			// Same gate as direct-tcpip (`ssh -L`): both are
			// client-driven local forwards.
			if !deps.AllowLocalForward {
				_ = newCh.Reject(ssh.Prohibited,
					"local port forwarding disabled by agent config")
				continue
			}
			go handleDirectStreamlocal(ctx, newCh, streamlocalAllow)
		default:
			_ = newCh.Reject(ssh.UnknownChannelType,
				"only session, direct-tcpip, and direct-streamlocal@openssh.com channels are supported")
		}
	}
}

// ServeLANListener accepts plain TCP connections on `ln` and feeds
// each one to handleSSHConn as a NEW SSH session. Blocks until ln
// errors or ctx is canceled. Use as `errgroup.Go(func() error {
// return ServeLANListener(gctx, ln, deps) })`.
//
// cloudboxVouched is always false here — LAN-direct callers haven't
// been vouched for by cloudbox, so the SSH PasswordCallback enforces
// the OS-password gate the same way it does for direct-loopback WS
// callers.
func ServeLANListener(ctx context.Context, ln net.Listener, deps sshHandlerDeps) error {
	currentUser, _ := hostauth.CurrentUser()
	authURL := strings.TrimSpace(deps.AuthURL)
	streamlocalAllow := buildStreamlocalAllowlist(deps.ForwardSockets)

	// Close the listener when ctx cancels so Accept() unblocks.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		go func(c net.Conn) {
			defer c.Close()
			handleSSHConn(ctx, c, c.RemoteAddr().String(), false /* never cloudbox-vouched on LAN */, deps, currentUser, authURL, streamlocalAllow)
		}(conn)
	}
}

// ServeLANSSH is the cmd/outpost-callable wrapper that builds the
// internal sshHandlerDeps from the public Deps shape and calls
// ServeLANListener. Lets the daemon main loop attach a LAN-direct SSH
// listener (FileConfig.SSHListenAddr) without main.go having to know
// the internal handler-deps fields.
//
// Always passes cloudboxVouched=false (inside ServeLANListener): the
// LAN TCP path has no upstream vouching, so the OS-password gate
// applies.
func ServeLANSSH(ctx context.Context, ln net.Listener, deps Deps) error {
	auth := deps.Auth
	if auth == nil {
		auth = hostauth.DefaultAuthenticator()
	}
	handlerDeps := sshHandlerDeps{
		HostKey:            deps.SSHHostKey,
		Auth:               auth,
		AuthURL:            deps.AuthURL,
		AllowLocalForward:  deps.SSHAllowLocalForward,
		AllowRemoteForward: deps.SSHAllowRemoteForward,
		AllowAgentForward:  deps.SSHAllowAgentForward,
		SFTPEnabled:        deps.SFTPEnabled,
		Peers:              deps.PeerHosts,
		ForwardSockets:     deps.SSHForwardSockets,
		CloudboxBase:       deps.CloudboxBase,
		CloudboxProtocol:   deps.CloudboxProtocol,
		AccessToken:        deps.AccessToken,
		SelfName:           deps.SelfName,
	}
	return ServeLANListener(ctx, ln, handlerDeps)
}

// ServeLANSSHWS mounts the same /ssh handler the loopback gin engine
// uses on a fresh HTTP server bound to ln. Replaces the plain-TCP
// ServeLANSSH path with a WS-mounted listener that accepts peer-ticket
// JWTs as the auth signal (the cookie itself never traverses the LAN).
//
// `deps.SSHTicketPubkey` + `deps.SSHTicketVerifier` + `deps.SSHTicketAudience`
// are the new wiring. With them set, a client that presents
// `Authorization: Bearer <peer-ticket>` on the WS upgrade verifies
// without a password prompt. Empty pubkey or nil verifier disables the
// path — the handler still mounts, but every connection falls through
// to the OS-password gate (matching the legacy LAN-TCP behavior).
//
// `X-Periscope-Role` is NOT trusted on this path (TrustPeriscopeRole=false)
// because a LAN listener that honored it would let any LAN device
// promote itself to admin by spoofing the header.
func ServeLANSSHWS(ctx context.Context, ln net.Listener, deps Deps) error {
	auth := deps.Auth
	if auth == nil {
		auth = hostauth.DefaultAuthenticator()
	}
	handlerDeps := sshHandlerDeps{
		HostKey:            deps.SSHHostKey,
		Auth:               auth,
		AuthURL:            deps.AuthURL,
		AllowLocalForward:  deps.SSHAllowLocalForward,
		AllowRemoteForward: deps.SSHAllowRemoteForward,
		AllowAgentForward:  deps.SSHAllowAgentForward,
		SFTPEnabled:        deps.SFTPEnabled,
		Peers:              deps.PeerHosts,
		ForwardSockets:     deps.SSHForwardSockets,
		CloudboxBase:       deps.CloudboxBase,
		CloudboxProtocol:   deps.CloudboxProtocol,
		AccessToken:        deps.AccessToken,
		SelfName:           deps.SelfName,
		TrustPeriscopeRole: false,
		TicketPubkey:       deps.SSHTicketPubkey,
		TicketVerifier:     deps.SSHTicketVerifier,
		TicketAudience:     deps.SSHTicketAudience,
		TicketScope:        "ssh",
	}

	// Minimal gin engine: only `/ssh` is mounted. We deliberately
	// don't reuse the loopback engine — apps, clipboard, /shell,
	// etc. should never appear on a LAN bind. This is the SSH
	// transport, nothing else.
	engine := gin.New()
	engine.GET("/ssh", sshHandler(handlerDeps))
	httpSrv := &http.Server{Handler: engine}

	// Tie listener lifetime to ctx.
	go func() {
		<-ctx.Done()
		_ = httpSrv.Close()
	}()
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
// peerDial is the bundle of cloudbox-dialing state handleDirectTCPIP
// needs in order to tunnel a paired-peer SSH dial through cloudbox's
// /h/<peer>/ssh WSS endpoint. Empty fields disable the peer-tunneled
// path; the handler falls back to net.Dial for loopback targets the
// same way it always did.
type peerDial struct {
	cloudboxBase     string
	cloudboxProtocol string
	accessToken      string
	// selfName is this outpost's own AgentName — sent as
	// X-Outpost-Peer-Origin so cloudbox's audit log can record
	// "(from=<sibling>, to=<dest>)" for every peer-tunneled dial.
	// Empty value is fine; cloudbox just won't have an origin name.
	selfName string
}

// enabled reports whether the peer-tunneled dial path is configured.
// Both the cloudbox URL and the access_token must be set — neither
// alone is enough to reach cloudbox successfully.
func (pd peerDial) enabled() bool {
	return strings.TrimSpace(pd.cloudboxBase) != "" && strings.TrimSpace(pd.accessToken) != ""
}

func handleDirectTCPIP(ctx context.Context, newCh ssh.NewChannel, peers *peerhosts.Registry, pd peerDial) {
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

	// Peer-tunneled path: when the target is a paired outpost (not
	// loopback) and looks like an SSH dial (port 22, the default), send
	// the bytes through cloudbox's /h/<peer>/ssh WSS endpoint instead
	// of attempting net.Dial. The dial would otherwise fall through to
	// LAN DNS, which usually doesn't resolve peer outpost names from
	// inside another peer machine. Cloudbox already has account-level
	// routing for /h/<peer>/ssh — see peerhosts and ssh-config — so
	// reusing it here turns `ssh -J novicortex novidesign` into the
	// same wss round-trip `outpost ssh-proxy novidesign` already does
	// from the operator's local host. Loopback targets keep the
	// pre-existing net.Dial path so `ssh -L 7777:localhost:5432` etc.
	// stay zero-overhead.
	if pd.enabled() && msg.PortToConnect == 22 && !isLoopbackDest(msg.HostToConnect) {
		bridgePeerDialThroughCloudbox(ctx, newCh, pd, msg)
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

// isLoopbackDest is the canonical-form match for the always-allowed
// loopback target set. Mirrors allowDirectTCPIPDest's switch — kept
// separate so peer-dial path doesn't accidentally tunnel a `127.0.0.1`
// target through cloudbox.
func isLoopbackDest(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// bridgePeerDialThroughCloudbox is the `ssh -J peerA peerB` second-hop
// implementation. Opens a WebSocket to cloudbox's /h/<peerB>/ssh
// endpoint with this outpost's own AccessToken as the bearer, then
// byte-bridges the WSS NetConn to the SSH channel exactly like the
// loopback dial path. End-to-end the operator's SSH client (running on
// peerA's upstream) does its SSH handshake on bytes that flow:
//
//	client → peerA outpost → peerA cloudbox WSS → peerB outpost SSH server
//
// The inner SSH handshake still runs against peerB's OS-password gate
// (or peerB-scoped matrix_elev cookie if the operator pre-elevated).
// peerA contributes no auth beyond its own access_token, which
// cloudbox accepts because the registry says peerB is in the same
// account.
func bridgePeerDialThroughCloudbox(ctx context.Context, newCh ssh.NewChannel, pd peerDial, msg directTCPIPMsg) {
	wsURL, err := buildPeerSSHWSURL(pd.cloudboxBase, pd.cloudboxProtocol, msg.HostToConnect)
	if err != nil {
		slog.Warn("direct-tcpip: build peer WSS URL", "peer", msg.HostToConnect, "err", err)
		_ = newCh.Reject(ssh.ConnectionFailed, "peer-dial: "+err.Error())
		return
	}

	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// X-Outpost-Peer-Dial: 1 tells cloudbox "this WSS is a sibling-outpost
	// ProxyJump second-hop, not an operator dial — go ahead and proxy
	// without a matrix_elev cookie, because the destination outpost's own
	// PasswordCallback will still run". Cloudbox uses this header to
	// distinguish the peer-transport path from an ordinary cookieless
	// operator dial (which still EAUTHREQUIREDs). Outpost stamps the
	// header unconditionally on peer dials; cloudbox is free to ignore
	// it during a rollout — the failure mode is the prior 403, not a
	// silent misroute.
	wsConn, resp, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization":         []string{"Bearer " + pd.accessToken},
			"X-Outpost-Peer-Dial":   []string{"1"},
			"X-Outpost-Peer-Origin": []string{pd.selfName},
		},
	})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		slog.Info("direct-tcpip: peer WSS dial failed",
			"peer", msg.HostToConnect, "status", status, "err", err)
		_ = newCh.Reject(ssh.ConnectionFailed,
			"peer-dial "+msg.HostToConnect+": "+err.Error())
		return
	}
	wsConn.SetReadLimit(-1)
	// websocket.NetConn binds the WS to a context. We let it ride the
	// handler ctx so a server shutdown cancels in-flight peer dials.
	upstream := websocket.NetConn(ctx, wsConn, websocket.MessageBinary)
	defer upstream.Close()

	ch, chReqs, aerr := newCh.Accept()
	if aerr != nil {
		slog.Warn("direct-tcpip: peer channel accept", "peer", msg.HostToConnect, "err", aerr)
		_ = wsConn.Close(websocket.StatusInternalError, "accept failed")
		return
	}
	defer ch.Close()
	go ssh.DiscardRequests(chReqs)

	slog.Info("direct-tcpip: peer-tunneled bridge open",
		"peer", msg.HostToConnect,
		"origin", msg.OriginatorIPAddress+":"+strconv.Itoa(int(msg.OriginatorPort)))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, ch)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ch, upstream)
		_ = ch.CloseWrite()
	}()
	wg.Wait()
}

// buildPeerSSHWSURL constructs the wss:// URL for cloudbox's
// /h/<peer>/ssh endpoint. Mirrors cmd/outpost/ssh.go:buildSSHWSURL
// shape-for-shape — duplicated rather than imported because that
// helper lives in the main package and the agent shouldn't pull
// cmd/outpost.
func buildPeerSSHWSURL(cloudboxBase, protocol, peer string) (string, error) {
	s := strings.TrimSpace(cloudboxBase)
	if s == "" {
		return "", fmt.Errorf("cloudbox base url is empty")
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	scheme := "ws"
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "wss":
		scheme = "wss"
	default:
		if strings.EqualFold(u.Scheme, "https") || strings.EqualFold(u.Scheme, "wss") {
			scheme = "wss"
		}
	}
	u.Scheme = scheme
	u.Path = strings.TrimRight(u.Path, "/") + "/matrix/h/" + url.PathEscape(peer) + "/ssh"
	return u.String(), nil
}

// directStreamlocalMsg is the channel-data payload SSH clients send when
// opening a `direct-streamlocal@openssh.com` channel — the OpenSSH
// extension behind unix-socket forwarding (used by `ssh -L
// localport:/remote.sock` and by podman's `ssh://` URL transport).
// Wire format per OpenSSH PROTOCOL §2.4: string socket_path, then two
// reserved fields for forward compatibility.
//
// The fields must be exported because ssh.Unmarshal reflects on them
// from this package — crypto/ssh's own internal mirror uses unexported
// fields only because it's the same package.
type directStreamlocalMsg struct {
	SocketPath string
	Reserved0  string
	Reserved1  uint32
}

// defaultStreamlocalAllowlist returns the always-allowed socket paths
// the SSH server will forward `direct-streamlocal@openssh.com` channels
// onto without operator action. The list is rebuilt fresh on each
// handler init (cheap) so a podman-machine restart that moves the
// socket gets picked up on the next outpost restart.
//
// Included by default:
//   - Every path probed by DetectPodman (rootless + system + macOS
//     machine paths) — the same set the admin UI's Podman toggle uses.
//   - Canonical docker sockets (`/var/run/docker.sock`,
//     `$HOME/.docker/run/docker.sock`).
//
// Anything else needs explicit opt-in via FileConfig.SSHForwardSockets.
// Future-proofing note: docker rides the SSH `exec` channel via
// `docker system dial-stdio` and doesn't actually need streamlocal
// today; the docker entries are pre-allowed so we don't have to chase
// a config edit if a future docker version switches transports.
func defaultStreamlocalAllowlist() []string {
	paths := podmanCandidates()
	paths = append(paths, "/var/run/docker.sock")
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".docker/run/docker.sock"))
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, filepath.Clean(p))
	}
	return out
}

// buildStreamlocalAllowlist merges the static defaults with the
// operator-supplied extension and canonicalizes every entry with
// filepath.Clean, so path-traversal forms (`/a/../b`) and the canonical
// form match the same allowlist entry.
func buildStreamlocalAllowlist(extra []string) []string {
	allow := defaultStreamlocalAllowlist()
	for _, p := range extra {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		allow = append(allow, filepath.Clean(p))
	}
	return allow
}

// allowStreamlocalDest reports whether path is in the allowlist. Match
// is exact-string after filepath.Clean (no globbing, no symlink
// resolution). Empty path always rejects — net.Dial("unix", "") returns
// a confusing error and we'd rather fail explicit here.
func allowStreamlocalDest(path string, allowlist []string) bool {
	if path == "" {
		return false
	}
	return slices.Contains(allowlist, filepath.Clean(path))
}

// handleDirectStreamlocal services one `direct-streamlocal@openssh.com`
// channel-open request: parse the payload, allowlist-check the socket
// path, dial it, and bidirectionally bridge bytes between the SSH
// channel and the unix socket. Mirror image of handleDirectTCPIP — same
// io.Copy + WaitGroup shape, just net.Dial("unix", ...) and
// *net.UnixConn for the CloseWrite cast.
//
// Trust model: same OS-password gate that protects session channels.
// The path allowlist further bounds reach (no arbitrary socket
// forwarding even if the OS user could otherwise reach it) so an SSH
// client can't pivot the agent into a generic socket-forwarder.
func handleDirectStreamlocal(ctx context.Context, newCh ssh.NewChannel, allowlist []string) {
	var msg directStreamlocalMsg
	if err := ssh.Unmarshal(newCh.ExtraData(), &msg); err != nil {
		slog.Warn("direct-streamlocal: bad payload", "err", err)
		_ = newCh.Reject(ssh.ConnectionFailed, "malformed direct-streamlocal payload")
		return
	}
	socketPath := filepath.Clean(strings.TrimSpace(msg.SocketPath))
	if !allowStreamlocalDest(socketPath, allowlist) {
		slog.Info("direct-streamlocal: refused socket not in allowlist", "socket", msg.SocketPath)
		_ = newCh.Reject(ssh.Prohibited,
			"socket not in allowlist (podman/docker defaults + SSHForwardSockets; got "+msg.SocketPath+")")
		return
	}

	d := net.Dialer{}
	upstream, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		slog.Info("direct-streamlocal: dial failed", "socket", socketPath, "err", err)
		_ = newCh.Reject(ssh.ConnectionFailed, "dial "+socketPath+": "+err.Error())
		return
	}
	defer upstream.Close()

	ch, chReqs, aerr := newCh.Accept()
	if aerr != nil {
		slog.Warn("direct-streamlocal: channel accept", "socket", socketPath, "err", aerr)
		return
	}
	defer ch.Close()
	// Streamlocal channels carry no channel requests — drain any
	// spurious ones to keep the crypto/ssh request goroutine clean.
	go ssh.DiscardRequests(chReqs)

	slog.Info("direct-streamlocal: bridging", "socket", socketPath)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, ch)
		if uc, ok := upstream.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
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
