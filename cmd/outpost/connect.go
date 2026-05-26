// `outpost connect <host>` is the CLI mirror of the Periscope launcher's
// "Connect" button: it runs the once-per-idle-window OS-password step
// that unlocks the host for subsequent SSH connections. POSTs to
// cloudbox's /h/:host/elevate endpoint, captures the returned
// matrix_elev cookie, and caches it on disk so later `outpost
// ssh-proxy` invocations (both human and agentic) can ride on it
// until idle / absolute expiry.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
)

func connectCmd() *cobra.Command {
	var (
		stdinFlag      bool
		userFlag       string
		keepAliveFlag  bool
		ttlFlag        string
		cookieOnlyFlag bool
	)
	cmd := &cobra.Command{
		Use:   "connect <host>",
		Short: "Unlock <host> for SSH by prompting for the host's OS password (mirrors the Connect button in the web UI)",
		Long: `Run this once per idle window. Prompts for the OS password of the
user that outpost runs as on <host>, then caches the cloudbox
matrix_elev cookie at ~/.cache/outpost/sessions/<host>.cookie. This
is the CLI equivalent of clicking the "Connect" button in the
Periscope launcher — same elevation flow, same cookie.

Both interactive ssh (via outpost ssh-proxy) and agentic tools then
read that cookie automatically — no further password prompts until
the elevation expires (1 h idle, 8 h absolute by default).

When stdin is not a TTY (agent context), pass --stdin to read the
password from stdin so the calling agent can supply it
programmatically.

Pass --keep-alive to hold the process open and ping cloudbox every
30 minutes. Each ping slides cloudbox's idle TTL forward (it slides
on any authed request past the halfway point), so the cookie stays
valid until the absolute 8 h cap. Useful for long-running agentic
flows that would otherwise hit EAUTHREQUIRED mid-run.

Pass --ttl to override cloudbox's absolute-expiry cap (default 8 h):
    default | <duration like 24h, 1h30m> | infinite
"infinite" is the JS-safe MaxInt sentinel (~285 years). The idle TTL
(1 h) still applies — combine with --keep-alive for a long-lived
session.

Pass --cookie-only to skip the password prompt entirely and go
straight to the keep-alive loop using an existing cached cookie.
Implies --keep-alive. Errors out if no cached cookie exists for
<host>. This is the supported pattern for daemonized supervision
(launchd / systemd): seed the cookie once with the interactive
"outpost connect --ttl infinite <host>", then daemonize
"outpost connect --cookie-only <host>" via "outpost run". The
daemon never needs the password — launchd-respawn-after-crash
just resumes pinging.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ttl, err := parseTTL(ttlFlag)
			if err != nil {
				return err
			}
			if cookieOnlyFlag {
				return runCookieOnlyKeepAlive(cmd.Context(), args[0])
			}
			return runConnect(cmd.Context(), args[0], userFlag, stdinFlag, keepAliveFlag, ttl)
		},
	}
	cmd.Flags().BoolVar(&stdinFlag, "stdin", false, "Read password from stdin instead of /dev/tty (for non-interactive callers)")
	cmd.Flags().StringVar(&userFlag, "user", "", "OS username on the remote host (default: the host's reported os_user, then $USER)")
	cmd.Flags().BoolVar(&keepAliveFlag, "keep-alive", false, "Stay running and ping every 30 min to slide the cookie's idle TTL")
	cmd.Flags().StringVar(&ttlFlag, "ttl", "", "Absolute-expiry override: default | <duration> | infinite")
	cmd.Flags().BoolVar(&cookieOnlyFlag, "cookie-only", false, "Skip the password prompt; ride an existing cached cookie into --keep-alive (daemon-friendly)")
	return cmd
}

// runCookieOnlyKeepAlive is the daemon-friendly entry point. Loads the
// previously-cached cookie + bearer, refuses if either is missing,
// and runs the keep-alive loop directly. Mirrors runConnect's prelude
// (config + bearer resolution) but skips every interactive surface
// (password prompt, user resolution, postElevate). A 401/403 from
// the ping loop exits non-zero so a supervisor wrapper can surface
// "re-elevate needed" to the operator out of band — that path
// already exists, the supervisor just sees the process exit and
// stops respawning (or respawns and re-fails, depending on policy).
func runCookieOnlyKeepAlive(ctx context.Context, host string) error {
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
	bearer := strings.TrimSpace(os.Getenv("OUTPOST_SESSION_JWT"))
	if bearer == "" {
		bearer = fc.AccessToken
	}
	if bearer == "" {
		bearer = fc.Token
	}
	if bearer == "" {
		return errors.New("no auth credential cached; re-pair with `outpost register`")
	}
	cookie, err := readCookie(host)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read cached cookie for %q: %w", host, err)
	}
	if cookie == "" {
		return fmt.Errorf("no cached cookie for %q; run `outpost connect --ttl infinite %s` first to seed one", host, host)
	}
	fmt.Fprintf(os.Stderr, "Keep-alive (cookie-only): pinging every 30 min until SIGTERM or absolute expiry.\n")
	return runKeepAlive(ctx, fc, bearer, host, cookie)
}

func runConnect(ctx context.Context, host, userFlag string, fromStdin, keepAlive bool, ttlSeconds int64) error {
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
	bearer := strings.TrimSpace(os.Getenv("OUTPOST_SESSION_JWT"))
	if bearer == "" {
		bearer = fc.AccessToken
	}
	if bearer == "" {
		bearer = fc.Token
	}
	if bearer == "" {
		return errors.New("no auth credential: re-pair with `outpost register`")
	}

	// Prompt for the password BEFORE the cloudbox round-trip. Resolving
	// the OS username via /api/v1/ssh/hosts (below) can take a beat over
	// slow links; doing it after readPassword means the operator sees
	// the prompt instantly, types the password, and then waits — far
	// less confusing than a silent gap before the prompt appears.
	password, err := readPassword(fmt.Sprintf("OS password for %s", host), fromStdin)
	if err != nil {
		return fmt.Errorf("read password: %w", err)
	}
	if password == "" {
		return errors.New("empty password")
	}

	// Resolve the OS username to elevate as. Preference order:
	//   1. --user explicit (operator override always wins)
	//   2. The host's reported os_user from cloudbox's /api/v1/ssh/hosts
	//      — this is the right default cross-machine, because the remote
	//      outpost's /auth gate compares the submitted username against
	//      *its own* OS user, not the caller's $USER.
	//   3. $USER (local) as a back-stop when (2) fails — better than
	//      hard-failing if cloudbox is briefly unreachable, and harmless
	//      when the operator's local username does happen to match.
	//   4. hostauth.CurrentUser() as a last resort on systems where $USER
	//      isn't set (cron, launchd-spawned shells, etc.).
	user := strings.TrimSpace(userFlag)
	if user == "" {
		if hosts, ferr := fetchSSHHosts(ctx, fc.ServerAddr, fc.ServerPort, fc.Protocol, bearer); ferr == nil {
			for _, h := range hosts {
				if strings.EqualFold(h.Host, host) && h.OsUser != "" {
					user = h.OsUser
					break
				}
			}
		}
	}
	if user == "" {
		user = strings.TrimSpace(os.Getenv("USER"))
	}
	if user == "" {
		user, _ = hostauth.CurrentUser()
	}
	if user == "" {
		return errors.New("could not determine OS username; pass --user")
	}

	cookie, err := postElevate(ctx, fc, bearer, host, user, password, ttlSeconds)
	if err != nil {
		return err
	}

	if err := writeCookie(host, cookie); err != nil {
		return fmt.Errorf("cache cookie: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Elevated %s. Cookie cached.\n", host)
	if !keepAlive {
		return nil
	}
	fmt.Fprintf(os.Stderr, "Keep-alive: pinging every 30 min until SIGTERM or absolute expiry.\n")
	return runKeepAlive(ctx, fc, bearer, host, cookie)
}

// runKeepAlive holds the process open, hitting /h/<host>/elev/ssh/ping
// every 30 minutes to slide the cloudbox idle TTL. Cloudbox issues a
// fresh Set-Cookie header when the cookie crosses its halfway-point
// (30 min into the 1 h window), so we capture the refreshed value and
// rewrite the cache file.
//
// Error handling distinguishes:
//   - Fatal (HTTP 401/403, or an explicit fatalPingError): the cookie
//     is dead (absolute expiry, revocation, JWT secret rotation). We
//     return non-zero so the supervisor knows to surface re-elevation
//     to the operator.
//   - Transient (network errors, 5xx, 408, 429, build/marshal failures
//     that could resolve themselves): retry with exponential backoff,
//     starting at 30 s and capping at 5 min between attempts. After
//     `keepAliveMaxConsecutiveFailures` (10) consecutive failures we
//     give up — at 5 min each that's ~50 min of total backoff, which
//     is well past any realistic cloudbox outage that wouldn't already
//     trigger a human response.
//
// On any successful ping the failure counter resets to zero, so a
// brief outage in the middle of a long session doesn't burn through
// the budget.
func runKeepAlive(ctx context.Context, fc *conf.FileConfig, bearer, host, cookie string) error {
	pingURL, err := buildPingURL(fc.ServerAddr, fc.ServerPort, fc.Protocol, host)
	if err != nil {
		return fmt.Errorf("build ping URL: %w", err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	current := cookie
	t := time.NewTicker(keepAliveInterval)
	defer t.Stop()
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "Keep-alive: exiting (%v).\n", ctx.Err())
			return nil
		case <-t.C:
		}
		next, err := pingElevate(ctx, client, pingURL, bearer, current)
		if err != nil {
			var fp fatalPingError
			if errors.As(err, &fp) {
				return fmt.Errorf("keep-alive ping (fatal): %w", err)
			}
			// Transient: retry-with-backoff inside this tick window
			// rather than waiting a full 30 min. Each attempt waits
			// retryBackoff(consecutiveFailures); after the run we
			// either succeeded (continue main loop) or exhausted
			// the budget (return).
			consecutiveFailures++
			if consecutiveFailures > keepAliveMaxConsecutiveFailures {
				return fmt.Errorf("keep-alive: gave up after %d consecutive transient errors (last: %w)",
					consecutiveFailures, err)
			}
			backoff := retryBackoff(consecutiveFailures)
			fmt.Fprintf(os.Stderr, "Keep-alive: transient ping error #%d, retrying in %s: %v\n",
				consecutiveFailures, backoff, err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			// Don't reset failure counter yet; the next loop iteration
			// will attempt the ping again. If it succeeds, we reset
			// below. If it fails again, we count up.
			continue
		}
		consecutiveFailures = 0
		if next != "" && next != current {
			if werr := writeCookie(host, next); werr != nil {
				return fmt.Errorf("rewrite cookie: %w", werr)
			}
			current = next
		}
	}
}

// keepAliveInterval is the ping cadence. Cloudbox slides the cookie past
// its halfway mark (30 min for a 1 h idle TTL), so pinging at 30 min is
// the largest safe gap. Smaller would also work; bigger lets the cookie
// briefly expire between pings.
var keepAliveInterval = 30 * time.Minute

// keepAliveMaxConsecutiveFailures bounds the retry budget for transient
// ping errors. At max-backoff (5 min) this is ~50 min of total wait,
// which is well past any realistic cloudbox blip that doesn't already
// warrant operator intervention.
var keepAliveMaxConsecutiveFailures = 10

// retryBackoff returns the wait duration before retry attempt n
// (1-indexed). 30s, 60s, 120s, 240s, 300s (cap), 300s, ... — a
// doubling sequence saturated at keepAliveBackoffCap. Returning a
// nonzero value for n=1 is important: the first failure shouldn't
// retry immediately — that'd hammer a flaky network or a cloudbox
// that's already overloaded.
func retryBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	base := 30 * time.Second
	d := base << (n - 1) // 30s, 60s, 120s, 240s, 480s...
	if d > keepAliveBackoffCap || d <= 0 /* overflow */ {
		d = keepAliveBackoffCap
	}
	return d
}

var keepAliveBackoffCap = 5 * time.Minute

// fatalPingError marks ping errors the keep-alive loop should NOT
// retry — auth has broken (401/403), the cookie is dead, the only
// recovery is operator re-elevation. Wrapping the HTTP status in a
// dedicated type lets runKeepAlive use errors.As to distinguish it
// from a 5xx / network blip that's worth retrying.
type fatalPingError struct {
	status int
	body   string
}

func (e fatalPingError) Error() string {
	return fmt.Sprintf("ping HTTP %d (cookie no longer valid): %s", e.status, e.body)
}

// pingElevate POSTs to the ping endpoint with the current cookie and
// returns the refreshed cookie value if cloudbox slid it (Set-Cookie
// in the response), else "" to indicate "no change". Distinguishes
// transient errors (returned as ordinary errors) from fatal auth
// failures (returned as fatalPingError) — see runKeepAlive for how
// the two are handled.
func pingElevate(ctx context.Context, client *http.Client, pingURL, bearer, cookie string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pingURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.AddCookie(&http.Cookie{Name: "matrix_elev", Value: cookie})
	resp, err := client.Do(req)
	if err != nil {
		// Transport-level error: DNS, dial refused, TLS, connection
		// reset, timeout — anything that's network-shaped. Retryable.
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		bodyStr := strings.TrimSpace(string(body))
		// 401/403 means the cookie is dead. 410 (host removed) is
		// also fatal. Everything else (408 request timeout, 429 too
		// many requests, 5xx server errors) is retryable.
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusGone:
			return "", fatalPingError{status: resp.StatusCode, body: bodyStr}
		}
		return "", fmt.Errorf("ping HTTP %d: %s", resp.StatusCode, bodyStr)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "matrix_elev" && ck.Value != "" {
			return ck.Value, nil
		}
	}
	// No Set-Cookie: cloudbox decided the cookie wasn't past its
	// halfway point yet (or middleware didn't slide for some reason).
	// Keep the existing one.
	return "", nil
}

// buildPingURL is the ping-endpoint analogue of buildElevateURL. Same
// host/scheme reasoning; the URL just gets an extra "/ping" segment.
func buildPingURL(server string, port int, protocol, host string) (string, error) {
	base, err := buildElevateURL(server, port, protocol, host)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(base, "/") + "/ping", nil
}

// readPassword reads a password from /dev/tty (echo off) or from stdin
// when fromStdin is true. The prompt string is shown to the TTY only.
// Falls back to a clear error when no TTY is available (agent context)
// so the caller knows to pass --stdin instead of hanging on a Read.
func readPassword(prompt string, fromStdin bool) (string, error) {
	if fromStdin {
		buf, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(buf), "\r\n"), nil
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("no /dev/tty available; pass --stdin to read from stdin (%w)", err)
	}
	defer tty.Close()
	fmt.Fprintf(tty, "%s: ", prompt)
	raw, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// postElevate hits cloudbox's /h/:host/elevate with the bearer + body
// cloudbox's Elevate handler expects. Captures the matrix_elev cookie
// from the Set-Cookie response header. ttlSeconds, when > 0, is sent
// as the absolute-cap override (`ttl_seconds`); 0 omits the field so
// cloudbox applies its default cap.
func postElevate(ctx context.Context, fc *conf.FileConfig, bearer, host, user, password string, ttlSeconds int64) (string, error) {
	elevateURL, err := buildElevateURL(fc.ServerAddr, fc.ServerPort, fc.Protocol, host)
	if err != nil {
		return "", err
	}
	payload := map[string]any{"user": user, "password": password}
	if ttlSeconds > 0 {
		payload["ttl_seconds"] = ttlSeconds
	}
	body, _ := json.Marshal(payload)

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, elevateURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("post %s: %w", elevateURL, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("elevation failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	// Defense in depth against URL-shape drift: cloudbox returns
	// Content-Type=application/json on the real elevate endpoint and
	// text/html for the SPA fallback. A 200 with HTML is what we got
	// when an old client posted to a path that didn't match any route,
	// and the bare "no cookie returned" error was unhelpful — the URL
	// is the actual bug. Surface it early.
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return "", fmt.Errorf("elevation reply was not JSON (Content-Type=%q at %s) — likely a cloudbox route mismatch", ct, elevateURL)
	}

	// Read Set-Cookie directly off the response instead of going through
	// a cookiejar. Cloudbox scopes the matrix_elev cookie's Path to the
	// data URL (/h/<host>/ssh) rather than the elevate URL we POSTed to
	// (/h/<host>/elev/ssh) — they're sibling paths, so net/http/cookiejar
	// correctly excludes the cookie from jar.Cookies(<elevateURL>), even
	// though it's right there in the response. Reading resp.Cookies()
	// directly bypasses the path-scoping check — appropriate because we
	// know which cookie name we want and which Path it'll be scoped to.
	for _, ck := range resp.Cookies() {
		if ck.Name == "matrix_elev" && ck.Value != "" {
			return ck.Value, nil
		}
	}
	return "", errors.New("server accepted credentials but returned no matrix_elev cookie")
}

// buildElevateURL constructs the http(s) URL for the SSH-builtin elevate
// endpoint from the server-addr fields cached in agent.json. The cloudbox
// route shape is `POST /h/<host>/elev/<builtin>` (not `/h/<host>/<builtin>/elevate`
// — the 410 handler that replaced the legacy /h/<host>/elevate hints at
// the latter, but the actual route uses `/elev/` as a literal segment to
// avoid colliding with gin's catch-all `*p` wildcard on /h/:host/app/:name).
// `outpost connect` only ever needed the cookie for the built-in /ssh
// endpoint, so the builtin is hard-coded to "ssh" here. Mirrors
// buildSSHWSURL in ssh.go but stays on http/https (POST is not a WS upgrade).
func buildElevateURL(server string, port int, protocol, host string) (string, error) {
	s := strings.TrimSpace(server)
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("parse server url %q: %w", server, err)
	}
	if strings.EqualFold(strings.TrimSpace(protocol), "wss") || strings.EqualFold(u.Scheme, "https") {
		u.Scheme = "https"
	} else {
		u.Scheme = "http"
	}
	if u.Port() == "" && port > 0 {
		u.Host = u.Hostname() + ":" + strconv.Itoa(port)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/h/" + url.PathEscape(host) + "/elev/ssh"
	return u.String(), nil
}

// sessionCookiePath returns the on-disk cache path for a given host.
func sessionCookiePath(host string) (string, error) {
	base, err := conf.DefaultCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// Sanitize: hostname can be anything cloudbox accepts; restrict the
	// filename to a known charset so a hostile name can't escape the
	// cache dir via path traversal.
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_' || r == '.':
			return r
		default:
			return '_'
		}
	}, host)
	return filepath.Join(dir, safe+".cookie"), nil
}

func writeCookie(host, cookie string) error {
	path, err := sessionCookiePath(host)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(cookie); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readCookie(host string) (string, error) {
	path, err := sessionCookiePath(host)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
