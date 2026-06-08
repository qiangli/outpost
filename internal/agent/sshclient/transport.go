// Transport: WebSocket dial to cloudbox's `/h/<host>/ssh` endpoint.
//
// This is the same handshake `outpost ssh-proxy` has used since day
// one — bearer + matrix_elev cookie + WSS upgrade — extracted into a
// shared package so the new in-process SSH client (cmd/outpost/
// ssh_tree.go and the MCP `outpost_ssh_exec` tool) can reuse it.
// `cmd/outpost/ssh.go` (ssh-proxy) keeps its existing thin wrapper
// that delegates here.
//
// Why the extraction: ssh-proxy treats the WS as a byte pipe and lets
// the user's `/usr/bin/ssh` do the SSH protocol on top. The new client
// path does the SSH protocol *in-process* via golang.org/x/crypto/ssh
// over the same byte pipe. Both paths need an identical dial — bearer
// resolution, elev-cookie attach, 401/403 retry semantics — so it
// belongs in one place.
package sshclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// ElevationCallback is invoked when cloudbox replies 401/403 to the
// WS upgrade — the caller's matrix_elev cookie is missing or stale.
// Returns a fresh cookie value on success; an error to give up.
//
// Interactive callers (the CLI) wire this to a /dev/tty password
// prompt that calls `runConnect`. Non-interactive callers (MCP tools,
// admincore.ExecSSH) pass nil — DialWS then surfaces a structured
// EAuthRequiredError instead of attempting recovery.
type ElevationCallback func(ctx context.Context, host string) (newCookie string, err error)

// DialOptions parameterizes the WS dial.
type DialOptions struct {
	// WSURL is the full `wss://cloudbox/h/<host>/ssh` URL — build with
	// BuildWSURL from the outpost's FileConfig.
	WSURL string

	// Bearer is the cloudbox access token. Resolved by the caller from
	// $OUTPOST_SESSION_JWT, fc.AccessToken, or fc.Token — the same
	// preference order ssh-proxy uses.
	Bearer string

	// Cookie is the currently-cached matrix_elev value (may be empty).
	Cookie string

	// PeerTicket, when set, swaps the dial onto the LAN-direct path:
	// the only header attached is `Authorization: Bearer <PeerTicket>`,
	// Cookie and the cloudbox Bearer are both omitted, and OnElevate
	// is never invoked (peer-ticket auth doesn't have an in-band
	// recovery path — the caller re-mints by re-running the
	// cookie→ticket exchange at cloudbox). Used when WSURL targets a
	// peer outpost's SSH-WS LAN listener directly, not cloudbox.
	PeerTicket string

	// Host is just the bare host name (the same value embedded in
	// WSURL). Used in error messages and threaded into OnElevate.
	Host string

	// OnElevate, if non-nil, is invoked once on the first 401/403 to
	// recover a fresh cookie. nil => surface EAuthRequiredError on the
	// first auth failure (the non-interactive policy). Ignored when
	// PeerTicket is set (no in-band recovery on the LAN-direct path).
	OnElevate ElevationCallback

	// DialTimeout caps each individual dial attempt. Default 30s.
	DialTimeout time.Duration
}

// DialWS opens the WebSocket to cloudbox/h/<host>/ssh, attaches Bearer
// + optional cookie, and (if OnElevate is set) recovers once from a
// 401/403 by minting a fresh cookie. The returned *websocket.Conn has
// ReadLimit disabled so SSH streams aren't artificially capped.
//
// Caller is responsible for wrapping the conn as a net.Conn via
// websocket.NetConn(ctx, conn, websocket.MessageBinary) when feeding
// it to ssh.NewClientConn.
func DialWS(ctx context.Context, opts DialOptions) (*websocket.Conn, error) {
	if opts.WSURL == "" {
		return nil, errors.New("sshclient: empty WSURL")
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 30 * time.Second
	}

	// LAN-direct peer-ticket path: one-shot dial with just the ticket
	// in Authorization. The receiver verifies the ticket locally; no
	// 401 retry/recover logic applies here (re-elev means going back
	// to cloudbox for a fresh ticket, which is the caller's job).
	if opts.PeerTicket != "" {
		dialCtx, cancel := context.WithTimeout(ctx, opts.DialTimeout)
		defer cancel()
		h := http.Header{}
		h.Set("Authorization", "Bearer "+opts.PeerTicket)
		conn, _, err := websocket.Dial(dialCtx, opts.WSURL, &websocket.DialOptions{HTTPHeader: h})
		if err != nil {
			return nil, fmt.Errorf("dial %s (peer-ticket): %w", opts.WSURL, err)
		}
		conn.SetReadLimit(-1)
		return conn, nil
	}

	dialOpts := func(cookie string) *websocket.DialOptions {
		h := http.Header{}
		if opts.Bearer != "" {
			h.Set("Authorization", "Bearer "+opts.Bearer)
		}
		if cookie != "" {
			h.Set("Cookie", "matrix_elev="+cookie)
		}
		return &websocket.DialOptions{HTTPHeader: h}
	}

	cookie := opts.Cookie
	// Two attempts max: the first with whatever cookie we have, the
	// second (only if OnElevate is set) with a freshly-minted one.
	for attempt := range 2 {
		dialCtx, cancel := context.WithTimeout(ctx, opts.DialTimeout)
		conn, resp, err := websocket.Dial(dialCtx, opts.WSURL, dialOpts(cookie))
		cancel()
		if err == nil {
			conn.SetReadLimit(-1)
			return conn, nil
		}
		// Only the elevation gate's 401/403 is worth retrying. DNS,
		// refused, TLS, etc. bubble up immediately.
		if resp == nil || (resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden) {
			return nil, fmt.Errorf("dial %s: %w", opts.WSURL, err)
		}
		// No recovery callback => fail fast with a structured error
		// agentic callers (MCP tools) can match on.
		if opts.OnElevate == nil {
			return nil, EAuthRequiredError{Host: opts.Host, Cause: err}
		}
		if attempt > 0 {
			return nil, EAuthRequiredError{Host: opts.Host, Cause: errors.New("retry budget exhausted")}
		}
		fresh, eerr := opts.OnElevate(ctx, opts.Host)
		if eerr != nil {
			return nil, fmt.Errorf("re-elevate %s: %w", opts.Host, eerr)
		}
		cookie = fresh
	}
	return nil, EAuthRequiredError{Host: opts.Host, Cause: errors.New("retry budget exhausted")}
}

// EAuthRequiredError is returned when the cloudbox elevation gate
// refused us and either no OnElevate was supplied (non-interactive
// caller) or the recovery attempt also failed. The structured shape
// lets agentic callers (MCP `outpost_ssh_exec`) report a precise
// "elevation required" condition instead of a opaque dial error.
type EAuthRequiredError struct {
	Host  string
	Cause error
}

func (e EAuthRequiredError) Error() string {
	return fmt.Sprintf("outpost: EAUTHREQUIRED for host %q — run `outpost connect %s` to elevate (%v)",
		e.Host, e.Host, e.Cause)
}

// Unwrap exposes the underlying dial error so errors.Is / errors.As
// work transparently for callers that want to inspect the cause.
func (e EAuthRequiredError) Unwrap() error { return e.Cause }

// BuildWSURL constructs the `ws(s)://<server>/h/<host>/ssh` URL from
// the outpost's FileConfig fields. server may be a bare hostname
// ("ai.dhnt.io"), host:port ("172.16.25.23:18080"), or a full URL
// ("https://example.com"). protocol is the matrix-tunnel transport
// returned by /api/register/exchange — "wss" => wss://, anything else
// => ws://. Same shape as `cmd/outpost/ssh.go`'s buildSSHWSURL —
// extracted here so both ssh-proxy and the new in-process client
// route through one definition.
func BuildWSURL(server string, port int, protocol, host string) (string, error) {
	s := strings.TrimSpace(server)
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("parse server url %q: %w", server, err)
	}
	scheme := "ws"
	if strings.EqualFold(strings.TrimSpace(protocol), "wss") {
		scheme = "wss"
	} else if strings.EqualFold(u.Scheme, "https") || strings.EqualFold(u.Scheme, "wss") {
		scheme = "wss"
	}
	u.Scheme = scheme
	if u.Port() == "" && port > 0 {
		u.Host = u.Hostname() + ":" + strconv.Itoa(port)
	}
	// Build u.Path with the *unescaped* host — url.URL.String() applies
	// path escaping on its own, so pre-escaping here would double-encode
	// (e.g. "host space" -> "host%2520space"). We do reject hostile bytes
	// upstream (cloudbox validates agent names), so leaving the encoding
	// to net/url is the right shape.
	u.Path = strings.TrimRight(u.Path, "/") + "/matrix/h/" + host + "/ssh"
	return u.String(), nil
}
