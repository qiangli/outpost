// SSH-target CRUD + one-shot Exec. These methods back both the
// `outpost ssh ...` CLI subtree and the `outpost_*_ssh_target` /
// `outpost_ssh_exec` MCP tools, keeping validation + filesystem
// access in one place.
//
// Targets are persisted as per-alias JSON files under
// $XDG_CONFIG_HOME/outpost/ssh/<name>.json (see conf/sshtargets.go).
// Mutation does NOT trigger admincore's restart-debounce — friendly
// aliases are pure-cache state.
//
// ExecSSH opens a fresh WS+SSH connection to cloudbox per call. Wave 1
// trades the per-call setup latency for simplicity; Wave 2 will add
// pooling if measurements show it matters.
package admincore

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/sshclient"
)

// SSHTargetView is the wire shape returned by list / upsert / show.
// Exactly the on-disk struct; defined as a separate name so we can
// add presentation-only fields later without breaking the file format.
type SSHTargetView = conf.SSHTarget

// ListSSHTargets enumerates configured aliases, sorted by name.
// Returns an empty slice (never nil) when nothing is configured.
func (s *Server) ListSSHTargets() ([]SSHTargetView, error) {
	ts, err := conf.ListSSHTargets()
	if err != nil {
		return nil, internalErr("list ssh targets: %s", err.Error())
	}
	if ts == nil {
		return []SSHTargetView{}, nil
	}
	return ts, nil
}

// GetSSHTarget returns one target by alias, or a 404 APIError.
func (s *Server) GetSSHTarget(name string) (SSHTargetView, error) {
	if err := conf.ValidSSHTargetName(name); err != nil {
		return SSHTargetView{}, badRequest("%s", err.Error())
	}
	t, err := conf.LoadSSHTarget(name)
	if err != nil {
		return SSHTargetView{}, notFound("%s", err.Error())
	}
	return *t, nil
}

// UpsertSSHTarget validates + persists. Idempotent.
//
// User is optional at upsert time — the caller can leave it blank and
// ExecSSH will return a clear "user not set" error at run time. The
// CLI typically resolves the OS username from cloudbox before calling
// here so the on-disk record carries everything needed.
func (s *Server) UpsertSSHTarget(t SSHTargetView) (SSHTargetView, error) {
	t.Name = strings.TrimSpace(t.Name)
	t.Host = strings.TrimSpace(t.Host)
	t.User = strings.TrimSpace(t.User)
	if err := conf.ValidSSHTargetName(t.Name); err != nil {
		return SSHTargetView{}, badRequest("%s", err.Error())
	}
	if t.Host == "" {
		return SSHTargetView{}, badRequest("host is required (paired host name in cloudbox)")
	}
	if err := conf.SaveSSHTarget(t); err != nil {
		return SSHTargetView{}, internalErr("save ssh target: %s", err.Error())
	}
	return t, nil
}

// DeleteSSHTarget is idempotent — no error when the alias doesn't
// exist (so a retry after a partial failure still succeeds).
func (s *Server) DeleteSSHTarget(name string) error {
	if err := conf.ValidSSHTargetName(name); err != nil {
		return badRequest("%s", err.Error())
	}
	if err := conf.DeleteSSHTarget(name); err != nil {
		return internalErr("delete ssh target: %s", err.Error())
	}
	return nil
}

// ExecSSHParams is the input shape for ExecSSH. Defaults match the
// constraints the MCP tool surfaces.
type ExecSSHParams struct {
	// Name is the configured target alias (`outpost ssh add <name>`).
	Name string

	// Command is the literal command line to run on the remote host.
	// Quoting / escaping is the caller's responsibility — this is
	// fed verbatim to `ssh.Session.Run`.
	Command string

	// JumpOverride, when non-empty, overrides the target's persisted
	// Via field for this one call (analogous to ssh's `-J <alias>`).
	// Use the empty string to honor the on-disk Via.
	JumpOverride string

	// Timeout caps the remote process's wall-clock runtime. Default
	// 60s; capped at 600s server-side to keep MCP callers from
	// holding the connection forever.
	Timeout time.Duration

	// MaxStdout / MaxStderr cap captured output. Default 1 MiB / 256 KiB.
	MaxStdout int64
	MaxStderr int64

	// Stdin, when non-nil, is fed to the remote process. The MCP
	// surface accepts base64-encoded bytes and constructs an
	// io.Reader here; CLI callers (outpost repair remote-binary,
	// etc.) can pass any io.Reader directly. Closed when copy
	// completes (sshclient does this).
	Stdin io.Reader
}

// ExecSSHResult is the output shape.
type ExecSSHResult struct {
	Stdout          []byte `json:"stdout"`
	Stderr          []byte `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
}

const (
	execSSHDefaultTimeout = 60 * time.Second
	execSSHMaxTimeout     = 10 * time.Minute
	execSSHDefaultStdout  = int64(1) << 20  // 1 MiB
	execSSHDefaultStderr  = int64(256) << 10 // 256 KiB
)

// ExecSSH resolves the target chain (including any Via hops), dials
// each leg, opens an in-process SSH client on the innermost
// connection, and runs Command.
//
// Errors map as follows:
//   - target missing                       → 404 NotFound
//   - any chain target.User empty          → 400 BadRequest with guidance
//   - elev cookie missing/stale            → 401 with EAUTHREQUIRED hint
//   - cloudbox unreachable / SSH handshake → 502 BadGateway
//   - timeout                              → wrapped as upstream() (502)
//   - remote exit-code != 0                → NOT an error — result is returned
//     with .ExitCode set; lets agents distinguish "command ran and failed"
//     from "couldn't get to the host."
func (s *Server) ExecSSH(ctx context.Context, p ExecSSHParams) (*ExecSSHResult, error) {
	if err := conf.ValidSSHTargetName(p.Name); err != nil {
		return nil, badRequest("%s", err.Error())
	}
	if strings.TrimSpace(p.Command) == "" {
		return nil, badRequest("command is required")
	}

	innerClient, cleanup, err := s.dialSSHChain(ctx, p.Name, p.JumpOverride)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Apply caps + defaults.
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = execSSHDefaultTimeout
	} else if timeout > execSSHMaxTimeout {
		timeout = execSSHMaxTimeout
	}
	maxStdout := p.MaxStdout
	if maxStdout <= 0 {
		maxStdout = execSSHDefaultStdout
	}
	maxStderr := p.MaxStderr
	if maxStderr <= 0 {
		maxStderr = execSSHDefaultStderr
	}

	res, runErr := innerClient.Exec(ctx, sshclient.ExecOptions{
		Command:   p.Command,
		Timeout:   timeout,
		MaxStdout: maxStdout,
		MaxStderr: maxStderr,
		Stdin:     p.Stdin,
	})
	if runErr != nil && res == nil {
		return nil, upstream("exec: %s", runErr.Error())
	}
	return &ExecSSHResult{
		Stdout:          res.Stdout,
		Stderr:          res.Stderr,
		ExitCode:        res.ExitCode,
		StdoutTruncated: res.StdoutTruncated,
		StderrTruncated: res.StderrTruncated,
	}, nil
}

// dialSSHChain dials the full target chain (outermost via cloudbox,
// then each subsequent hop via direct-tcpip) and returns the
// innermost ssh.Client. The cleanup func closes each client in
// reverse order — call it to tear down the whole chain.
//
// This is shared between admincore.ExecSSH and any future MCP-driven
// methods (interactive subsystems, sftp, port-forwarding) — the
// chain semantics belong here, not duplicated per verb.
func (s *Server) dialSSHChain(ctx context.Context, name, jumpOverride string) (*sshclient.Client, func(), error) {
	chain, err := conf.ResolveSSHTargetChain(name, jumpOverride)
	if err != nil {
		return nil, nil, notFound("%s", err.Error())
	}
	// Validate users along the chain up front so we fail fast before
	// any network I/O.
	for _, t := range chain {
		if strings.TrimSpace(t.User) == "" {
			return nil, nil, badRequest("target %q in chain has no user set — run `outpost ssh add %s --host %s --user <os_user>`",
				t.Name, t.Name, t.Host)
		}
	}

	fc, err := s.loadConfig()
	if err != nil {
		return nil, nil, internalErr("load config: %s", err.Error())
	}
	if fc.ServerAddr == "" {
		return nil, nil, unavailable("local outpost is not paired with cloudbox yet")
	}
	bearer := strings.TrimSpace(fc.AccessToken)
	if bearer == "" {
		bearer = strings.TrimSpace(fc.Token)
	}
	if bearer == "" {
		return nil, nil, unavailable("no cloudbox bearer cached — re-pair with `outpost register`")
	}

	knownHostsPath, err := conf.KnownHostsPath()
	if err != nil {
		return nil, nil, internalErr("known_hosts path: %s", err.Error())
	}

	clients := make([]*sshclient.Client, 0, len(chain))
	closeAll := func() {
		for i := len(clients) - 1; i >= 0; i-- {
			_ = clients[i].Close()
		}
	}

	// Leg 0: cloudbox WS dial to the outermost target, OR a plain TCP
	// dial when the target is Direct (LAN-direct path).
	outer := chain[0]
	var (
		outerTransport net.Conn
		outerDialErr   error
	)
	if outer.Direct {
		port := outer.Port
		if port <= 0 {
			port = conf.DefaultSSHPort
		}
		outerTransport, outerDialErr = net.DialTimeout("tcp", net.JoinHostPort(outer.Host, fmt.Sprintf("%d", port)), 30*time.Second)
		if outerDialErr != nil {
			return nil, nil, upstream("lan-direct dial %s:%d: %s", outer.Host, port, outerDialErr.Error())
		}
	} else {
		wsURL, err := sshclient.BuildWSURL(fc.ServerAddr, fc.ServerPort, fc.Protocol, outer.Host)
		if err != nil {
			return nil, nil, internalErr("build ws url: %s", err.Error())
		}
		cookie, _ := conf.ReadSessionCookie(outer.Host)
		dialCtx, dialCancel := context.WithTimeout(ctx, 35*time.Second)
		wsConn, derr := sshclient.DialWS(dialCtx, sshclient.DialOptions{
			WSURL:  wsURL,
			Bearer: bearer,
			Cookie: cookie,
			Host:   outer.Host,
		})
		dialCancel()
		if derr != nil {
			var eauth sshclient.EAuthRequiredError
			if asEAuth(derr, &eauth) {
				return nil, nil, &APIError{
					Status: http.StatusUnauthorized,
					Msg:    fmt.Sprintf("elevation required for %q — run `outpost connect %s`", outer.Host, outer.Host),
				}
			}
			return nil, nil, upstream("dial cloudbox: %s", derr.Error())
		}
		outerTransport = sshclient.AsNetConn(ctx, wsConn)
	}

	hostKeyCB, err := sshclient.KnownHostsCallbackTOFU(knownHostsPath, sshclient.HostAliasForHost(outer.Host))
	if err != nil {
		_ = outerTransport.Close()
		return nil, nil, internalErr("known_hosts callback: %s", err.Error())
	}
	outerHandshakeStart := time.Now()
	cli, err := sshclient.Dial(ctx, sshclient.Config{
		Transport:       outerTransport,
		HostAlias:       sshclient.HostAliasForHost(outer.Host),
		User:            outer.User,
		HostKeyCallback: hostKeyCB,
	})
	if err != nil {
		return nil, nil, upstream("ssh handshake (outer %s): %s", outer.Host, err.Error())
	}
	recordEdge(outer, outerHandshakeStart)
	clients = append(clients, cli)

	// Hops 1..N: open direct-tcpip through the prior client, then
	// layer another SSH session on top.
	for i := 1; i < len(chain); i++ {
		hop := chain[i]
		port := hop.Port
		if port <= 0 {
			port = conf.DefaultSSHPort
		}
		prior := clients[i-1]
		hopConn, err := prior.DirectTCPIP(ctx, hop.Host, port)
		if err != nil {
			closeAll()
			return nil, nil, upstream("direct-tcpip to %s:%d via %s: %s", hop.Host, port, chain[i-1].Name, err.Error())
		}
		hopCB, err := sshclient.KnownHostsCallbackTOFU(knownHostsPath, sshclient.HostAliasForHost(hop.Host))
		if err != nil {
			_ = hopConn.Close()
			closeAll()
			return nil, nil, internalErr("known_hosts callback: %s", err.Error())
		}
		hopHandshakeStart := time.Now()
		hopCli, err := sshclient.Dial(ctx, sshclient.Config{
			Transport:       hopConn,
			HostAlias:       sshclient.HostAliasForHost(hop.Host),
			User:            hop.User,
			HostKeyCallback: hopCB,
		})
		if err != nil {
			closeAll()
			return nil, nil, upstream("ssh handshake (hop %s): %s", hop.Host, err.Error())
		}
		recordEdge(hop, hopHandshakeStart)
		clients = append(clients, hopCli)
	}

	return clients[len(clients)-1], closeAll, nil
}

// asEAuth is a tiny errors.As alias localized here so we don't pull
// the errors package into the import list for one line at the call
// site.
func asEAuth(err error, target *sshclient.EAuthRequiredError) bool {
	for e := err; e != nil; {
		if v, ok := e.(sshclient.EAuthRequiredError); ok {
			*target = v
			return true
		}
		type unwrapper interface{ Unwrap() error }
		uw, ok := e.(unwrapper)
		if !ok {
			return false
		}
		e = uw.Unwrap()
	}
	return false
}
