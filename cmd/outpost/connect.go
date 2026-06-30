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

For a host SHARED with your account (not owned by it) there is no
password prompt at all: the share grant is the authority, and
cloudbox mints the cookie from it directly.

When stdin is not a TTY (agent context), pass --stdin to read the
password from stdin so the calling agent can supply it
programmatically.

Pass --keep-alive to hold the process open and ping cloudbox every
20 minutes. Each ping slides cloudbox's idle TTL forward (it slides
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
		// Elevation failures (wrong password, host offline) are not
		// usage errors — keep the message readable.
		SilenceUsage: true,
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
	fmt.Fprintf(os.Stderr, "Keep-alive (cookie-only): pinging every 20 min until SIGTERM or absolute expiry.\n")
	// Empty password/user signals the loop has no creds for auto-renew on
	// fatal auth. The existing fatal-exit behavior is preserved for this
	// daemon-friendly path: a supervisor (launchd / systemd) is expected
	// to surface "re-elevation needed" by noticing the non-zero exit.
	return runKeepAlive(ctx, fc, bearer, host, cookie, "", "", 0)
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

	// Resolve the host's cloudbox view (os_user + shared flag) BEFORE
	// any password prompt. For a host shared with this account (not
	// owned), cloudbox's elevate handler mints the cookie from the
	// HostShare row and never consults the password — the sharee
	// doesn't have the owner's OS password, and prompting for one
	// would be both confusing and pointless. Best-effort: when the
	// lookup fails (cloudbox briefly unreachable, old cloudbox without
	// the endpoint) fall through to the owner flow and prompt.
	var hostEntry *sshHostEntry
	if hosts, ferr := fetchSSHHosts(ctx, fc.ServerAddr, fc.ServerPort, fc.Protocol, bearer); ferr == nil {
		for i, h := range hosts {
			if strings.EqualFold(h.Host, host) {
				hostEntry = &hosts[i]
				break
			}
		}
	}
	shared := hostEntry != nil && hostEntry.Shared

	password := ""
	if !shared {
		var perr error
		password, perr = readPassword(fmt.Sprintf("OS password for %s", host), fromStdin)
		if perr != nil {
			return fmt.Errorf("read password: %w", perr)
		}
		if password == "" {
			return errors.New("empty password")
		}
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
	// Sharee elevations don't authenticate against the remote /auth gate
	// at all (the share row is the authority), so an unresolvable
	// username is only fatal on the owner path.
	user := strings.TrimSpace(userFlag)
	if user == "" && hostEntry != nil {
		user = strings.TrimSpace(hostEntry.OsUser)
	}
	if user == "" {
		user = strings.TrimSpace(os.Getenv("USER"))
	}
	if user == "" {
		user, _ = hostauth.CurrentUser()
	}
	if user == "" && !shared {
		return errors.New("could not determine OS username; pass --user")
	}

	cookie, err := postElevate(ctx, fc, bearer, host, user, password, ttlSeconds)
	if err != nil {
		return err
	}

	if err := writeCookie(host, cookie); err != nil {
		return fmt.Errorf("cache cookie: %w", err)
	}
	if shared {
		fmt.Fprintf(os.Stderr, "Elevated %s via your share grant (no OS password needed). Cookie cached.\n", host)
	} else {
		fmt.Fprintf(os.Stderr, "Elevated %s. Cookie cached.\n", host)
	}
	if !keepAlive {
		return nil
	}
	fmt.Fprintf(os.Stderr, "Keep-alive: pinging every 20 min until SIGTERM or absolute expiry.\n")
	return runKeepAlive(ctx, fc, bearer, host, cookie, password, user, ttlSeconds)
}

// runKeepAlive holds the process open, hitting /h/<host>/elev/ssh/ping
// every 20 minutes to slide the cloudbox idle TTL. Cloudbox issues a
// fresh Set-Cookie header once the cookie is past its halfway point
// (< 30 min left of the 1 h window), so we capture the refreshed value
// and rewrite the cache file.
//
// When `password` and `user` are non-empty the loop is "self-healing":
// on a fatalPingError that the disk-cookie self-heal couldn't resolve,
// the loop sleeps keepAliveSettleDelay (to absorb spurious 401s during
// a half-rolled-out cloudbox deploy), retries the ping once, and if
// still fatal POSTs a fresh elevation using the in-RAM credentials. A
// successful re-elevate rewrites the cookie and the loop continues —
// the operator sees no interruption. The only conditions that exit
// the loop in self-heal mode are SIGTERM/SIGINT and a fatalElevateError
// from postElevate (the OS password was actually rotated).
//
// When `password` is empty the loop runs in the original "cookie-only"
// mode used by `outpost connect --cookie-only`: any fatalPingError that
// disk-cookie self-heal can't resolve causes a non-zero exit so a
// supervisor (launchd / systemd) can surface re-elevation to the user.
// Transient errors still get retried, but the
// keepAliveMaxConsecutiveFailures (10) budget is enforced so a wedged
// daemon eventually exits.
//
// On any successful ping the failure counter resets to zero, so a
// brief outage in the middle of a long session doesn't burn through
// the budget.
func runKeepAlive(ctx context.Context, fc *conf.FileConfig, bearer, host, cookie, password, user string, ttlSeconds int64) error {
	pingURL, err := buildPingURL(fc.ServerAddr, fc.ServerPort, fc.Protocol, host)
	if err != nil {
		return fmt.Errorf("build ping URL: %w", err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	current := cookie
	// canSelfHeal == true means the loop holds OS credentials in RAM and
	// can auto-renew the elevation cookie on fatal auth. This flips two
	// behaviors at once: (a) fatalPingError after the disk-cookie retry
	// triggers settle-delay + re-elevate instead of exiting, and (b) the
	// transient-error budget is uncapped (because we no longer need a
	// supervisor to restart us — we can wait out arbitrary outages).
	canSelfHeal := password != "" && user != ""
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
				// Self-heal step 1: another process (interactive ssh's
				// slide-refresh, a manual `outpost connect`, a second
				// keepalive elsewhere) may have rewritten the disk cookie
				// since we last read it. Retry once with the disk value
				// before escalating. Observed 2026-05-28 when keepalive
				// jobs flapped repeatedly on 403 though the cookie file
				// on disk had been refreshed.
				if disk, derr := readCookie(host); derr == nil && disk != "" && disk != current {
					fmt.Fprintf(os.Stderr, "Keep-alive: ping 403 with in-memory cookie; retrying with refreshed disk cookie.\n")
					current = disk
					next, err = pingElevate(ctx, client, pingURL, bearer, current)
					if err == nil {
						goto pingSucceeded
					}
					// Either disk cookie also rejected (still fp) or
					// disk-retry hit a transient. Fall through.
				}
				// Self-heal step 2: re-elevate using in-RAM creds.
				// Only available when `password` and `user` were threaded
				// in. The settle delay absorbs spurious 401s during a
				// half-rolled-out cloudbox deploy: if cloudbox is in the
				// middle of swapping pods, a single ping may have hit the
				// pod-being-torn-down before the LB caught up. One more
				// ping after the delay distinguishes a real expired
				// cookie from a flapping deploy.
				if canSelfHeal && errors.As(err, &fp) {
					fmt.Fprintf(os.Stderr, "Keep-alive: fatal ping (%v); waiting %s to absorb deploy churn, then retrying.\n",
						err, keepAliveSettleDelay)
					select {
					case <-ctx.Done():
						return nil
					case <-time.After(keepAliveSettleDelay):
					}
					next, err = pingElevate(ctx, client, pingURL, bearer, current)
					if err == nil {
						goto pingSucceeded
					}
					if errors.As(err, &fp) {
						// Still fatal after the settle. Re-elevate.
						fmt.Fprintf(os.Stderr, "Keep-alive: re-elevating after fatal ping (cookie expired, revoked, or JWT_SECRET rotated).\n")
						fresh, perr := postElevate(ctx, fc, bearer, host, user, password, ttlSeconds)
						if perr != nil {
							var fe fatalElevateError
							if errors.As(perr, &fe) {
								// Credentials genuinely rejected by the
								// remote /auth gate — OS password rotated.
								// Stop the loop so the operator notices.
								return fmt.Errorf("keep-alive: re-elevate rejected (OS password may have changed): %w", perr)
							}
							// Transient during re-elevate (5xx, network,
							// content-type mismatch). Treat as a transient
							// loop error and let backoff handle it.
							err = perr
						} else {
							if werr := writeCookie(host, fresh); werr != nil {
								return fmt.Errorf("rewrite cookie after re-elevate: %w", werr)
							}
							current = fresh
							next = ""
							fmt.Fprintf(os.Stderr, "Keep-alive: re-elevated successfully; cookie refreshed.\n")
							goto pingSucceeded
						}
					}
					// err is now transient — fall through to backoff.
				}
				// Couldn't self-heal. If err is still fatal, exit; the
				// cookie-only path lands here whenever the disk-cookie
				// retry didn't recover.
				if errors.As(err, &fp) {
					return fmt.Errorf("keep-alive ping (fatal): %w", err)
				}
				// err is transient — fall through.
			}
			// Transient: retry-with-backoff inside this tick window
			// rather than waiting a full 30 min. Each attempt waits
			// retryBackoff(consecutiveFailures); after the run we
			// either succeeded (continue main loop) or exhausted
			// the budget (return).
			consecutiveFailures++
			// Uncap the transient budget when self-healing — there's no
			// supervisor on the other side to restart us, and a 30-min
			// cloudbox outage shouldn't terminate a session that can
			// recover the moment cloudbox returns. Cookie-only mode
			// keeps the cap so a wedged daemon eventually exits.
			if !canSelfHeal && consecutiveFailures > keepAliveMaxConsecutiveFailures {
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
	pingSucceeded:
		consecutiveFailures = 0
		if next != "" && next != current {
			if werr := writeCookie(host, next); werr != nil {
				return fmt.Errorf("rewrite cookie: %w", werr)
			}
			current = next
		}
	}
}

// keepAliveInterval is the ping cadence. Cloudbox only slides the cookie once
// it's in the SECOND HALF of its 1 h idle TTL (the refresh fires when < 30 min
// remain), so a ping must land inside that window with margin. Pinging at 30 min
// lands EXACTLY on the boundary — it slid only by the thin margin of network
// latency and missed under any timing jitter, forcing needless re-elevations
// around each hour. 20 min (TTL/3) puts a ping at ~40 min with ~20 min to spare,
// so the slide reliably fires every cycle. Smaller still works; never use the
// exact halfway point.
var keepAliveInterval = 20 * time.Minute

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

// keepAliveSettleDelay is how long the auto-renew branch waits before
// retrying the ping (and then re-elevating) when a fatalPingError comes
// back and we hold creds in RAM. A half-redeployed cloudbox can emit a
// spurious 401/403 mid-rolling-deploy; sleeping ~settleDelay lets the
// fleet stabilize before we burn a fresh OS-password POST. Long enough
// to outlast a typical k8s rolling-deploy probe window; short enough
// that a real expired cookie still recovers within a minute.
var keepAliveSettleDelay = 30 * time.Second

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

// fatalElevateError marks a postElevate response that's a credential
// rejection (HTTP 401 or 403): the OS password no longer works for the
// remote host's auth gate. Distinct from a 5xx / transport-blip failure
// from postElevate, which is treated as transient by the keep-alive
// auto-renew loop. Mirrors fatalPingError so runKeepAlive can use
// errors.As to discriminate cleanly.
type fatalElevateError struct {
	status int
	body   string
}

func (e fatalElevateError) Error() string {
	return fmt.Sprintf("elevate HTTP %d (credentials rejected): %s", e.status, e.body)
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
		body := strings.TrimSpace(string(respBody))
		// 401 / 403 = the OS password the caller submitted is no longer
		// accepted by the remote host's /auth gate. Surface this as a
		// typed fatal so the keep-alive auto-renew loop can distinguish
		// it from a transient 5xx / cloudbox blip and stop the loop
		// instead of looping forever on a bad password.
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "", fatalElevateError{status: resp.StatusCode, body: body}
		}
		return "", fmt.Errorf("elevation failed (HTTP %d): %s", resp.StatusCode, body)
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
	u.Path = strings.TrimRight(u.Path, "/") + "/matrix/h/" + url.PathEscape(host) + "/elev/ssh"
	return u.String(), nil
}

// writeCookie / readCookie are thin wrappers around the exported
// helpers in `internal/agent/conf/sessions.go`. The concrete
// implementation moved into the conf package so admincore-driven MCP
// tools (e.g. outpost_ssh_exec) can read the same cache without
// re-importing cmd/outpost.
func writeCookie(host, cookie string) error  { return conf.WriteSessionCookie(host, cookie) }
func readCookie(host string) (string, error) { return conf.ReadSessionCookie(host) }
