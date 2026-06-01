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

	// Timeout caps the remote process's wall-clock runtime. Default
	// 60s; capped at 600s server-side to keep MCP callers from
	// holding the connection forever.
	Timeout time.Duration

	// MaxStdout / MaxStderr cap captured output. Default 1 MiB / 256 KiB.
	MaxStdout int64
	MaxStderr int64
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

// ExecSSH resolves the target alias, dials cloudbox over the matrix
// tunnel, opens an in-process SSH client, and runs Command.
//
// Errors map as follows:
//   - target missing                       → 404 NotFound
//   - target.User empty                    → 400 BadRequest with guidance
//   - elev cookie missing/stale            → 401 with EAUTHREQUIRED hint
//   - cloudbox unreachable / SSH handshake → 502 BadGateway
//   - timeout                              → 504 (wrapped in APIError? we use upstream())
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

	target, err := conf.LoadSSHTarget(p.Name)
	if err != nil {
		return nil, notFound("%s", err.Error())
	}
	if target.User == "" {
		return nil, badRequest("target %q has no user set — run `outpost ssh add %s --host %s --user <os_user>`",
			p.Name, p.Name, target.Host)
	}

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

	// Resolve transport parameters from FileConfig (the same source
	// ssh-proxy uses).
	fc, err := s.loadConfig()
	if err != nil {
		return nil, internalErr("load config: %s", err.Error())
	}
	if fc.ServerAddr == "" {
		return nil, unavailable("local outpost is not paired with cloudbox yet")
	}
	bearer := strings.TrimSpace(fc.AccessToken)
	if bearer == "" {
		bearer = strings.TrimSpace(fc.Token)
	}
	if bearer == "" {
		return nil, unavailable("no cloudbox bearer cached — re-pair with `outpost register`")
	}

	wsURL, err := sshclient.BuildWSURL(fc.ServerAddr, fc.ServerPort, fc.Protocol, target.Host)
	if err != nil {
		return nil, internalErr("build ws url: %s", err.Error())
	}
	cookie, _ := conf.ReadSessionCookie(target.Host)

	// Dial WS. No OnElevate — admincore is server-side and cannot
	// prompt for a password. Surface EAUTHREQUIRED as a structured
	// 401 so MCP callers can guide the operator.
	dialCtx, dialCancel := context.WithTimeout(ctx, 35*time.Second)
	wsConn, err := sshclient.DialWS(dialCtx, sshclient.DialOptions{
		WSURL:  wsURL,
		Bearer: bearer,
		Cookie: cookie,
		Host:   target.Host,
	})
	dialCancel()
	if err != nil {
		var eauth sshclient.EAuthRequiredError
		if asEAuth(err, &eauth) {
			return nil, &APIError{
				Status: http.StatusUnauthorized,
				Msg:    fmt.Sprintf("elevation required for %q — run `outpost connect %s`", target.Host, target.Host),
			}
		}
		return nil, upstream("dial cloudbox: %s", err.Error())
	}
	netConn := sshclient.AsNetConn(ctx, wsConn)

	knownHostsPath, err := conf.KnownHostsPath()
	if err != nil {
		_ = netConn.Close()
		return nil, internalErr("known_hosts path: %s", err.Error())
	}
	hostKeyCB, err := sshclient.KnownHostsCallbackTOFU(knownHostsPath, sshclient.HostAliasForHost(target.Host))
	if err != nil {
		_ = netConn.Close()
		return nil, internalErr("known_hosts callback: %s", err.Error())
	}

	client, err := sshclient.Dial(ctx, sshclient.Config{
		Transport:       netConn,
		HostAlias:       sshclient.HostAliasForHost(target.Host),
		User:            target.User,
		HostKeyCallback: hostKeyCB,
	})
	if err != nil {
		return nil, upstream("ssh handshake: %s", err.Error())
	}
	defer client.Close()

	res, runErr := client.Exec(ctx, sshclient.ExecOptions{
		Command:   p.Command,
		Timeout:   timeout,
		MaxStdout: maxStdout,
		MaxStderr: maxStderr,
	})
	if runErr != nil && res == nil {
		return nil, upstream("exec: %s", runErr.Error())
	}
	out := &ExecSSHResult{
		Stdout:          res.Stdout,
		Stderr:          res.Stderr,
		ExitCode:        res.ExitCode,
		StdoutTruncated: res.StdoutTruncated,
		StderrTruncated: res.StderrTruncated,
	}
	// runErr non-nil means the session terminated with an error AFTER
	// producing some output (e.g. exit code != 0). The structured
	// result is the agent-visible part; the error itself is logged
	// only.
	return out, nil
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
