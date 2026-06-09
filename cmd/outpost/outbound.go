// `outpost outbound …` is the CLI mirror of the admin UI's Outbound
// section. It drives the running local outpost's admin-UI HTTP API on
// 127.0.0.1:17777 — it does NOT reimplement the elevate/pinger logic.
// All commands assume the local outpost is already running (`outpost
// start`) and paired; the binary will print a friendly hint if the
// admin UI is unreachable.
//
// Auth model: each invocation reads ~/.cache/outpost/admin.cookie. If
// missing or expired (1h TTL, wiped on outpost restart), commands print
// "run `outpost outbound login` first" and exit non-zero. Login itself
// prompts for the LOCAL OS password (same gate as the admin UI's login
// page); `connect` prompts for the REMOTE host's OS password (the one
// cloudbox's elevate endpoint will verify).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
)

// ttlInfiniteSeconds is the wire sentinel for "no absolute cap". We use
// Number.MAX_SAFE_INTEGER (2^53-1) rather than math.MaxInt64 so the
// admin UI's JS, which represents numbers as float64, can round-trip
// the value cleanly through JSON. The CLI agrees on the same sentinel
// so both clients send identical payloads for --ttl infinite.
const ttlInfiniteSeconds int64 = 1<<53 - 1

func outboundCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "outbound",
		Short: "Manage outbound mounts to peer outposts (CLI mirror of the admin UI)",
		Long: `Outbound mounts let this machine reach apps and services running
on other paired outposts through cloudbox. Most subcommands (login, logout,
list, add, rm, connect, disconnect) drive the running local outpost's admin
UI API on 127.0.0.1:17777 and require an admin-cookie 'login' first.
'outbound suggest' takes a different path: it goes through the MCP bearer
endpoint at /mcp/ and does NOT require login.

Two passwords are involved across the cookie-based subcommands:
  - "outpost outbound login" takes the LOCAL OS password (admin-UI session).
  - "outpost outbound connect" takes the REMOTE host's OS password
    (cloudbox elevates the matrix_elev cookie scoped to that host+app).`,
	}
	cmd.AddCommand(
		outboundLoginCmd(),
		outboundLogoutCmd(),
		outboundListCmd(),
		outboundAddCmd(),
		outboundConnectCmd(),
		outboundDisconnectCmd(),
		outboundRmCmd(),
		outboundSuggestCmd(),
	)
	return cmd
}

// outboundSuggestCmd lists the (host, app) pairs cloudbox will let
// this account mount. Backed by the MCP server (bearer token from
// FileConfig) rather than the session-cookie REST path the rest of
// this subcommand tree uses — no separate `outpost outbound login`
// needed. Read-only; agents and humans share the same backend.
func outboundSuggestCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "suggest",
		Short: "List remote (host, app) pairs you could mount as an outbound (auth via MCP bearer token, no `login` required)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				Suggestions []struct {
					Host         string `json:"host"`
					OsUser       string `json:"os_user,omitempty"`
					Name         string `json:"name"`
					Scheme       string `json:"scheme,omitempty"`
					RequireLogin bool   `json:"require_login"`
					IndexPath    string `json:"index_path,omitempty"`
					Title        string `json:"title,omitempty"`
					Online       bool   `json:"online"`
					Shared       bool   `json:"shared,omitempty"`
				} `json:"suggestions"`
			}
			if err := session.callTool(cmd.Context(), "outpost_suggest_outbound", map[string]any{}, &out); err != nil {
				return err
			}
			if jsonOut {
				b, _ := json.MarshalIndent(out, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if len(out.Suggestions) == 0 {
				fmt.Println("No remote (host, app) pairs are visible to this account.")
				return nil
			}
			fmt.Printf("%-18s  %-12s  %-18s  %-6s  %s\n", "HOST", "OS_USER", "APP", "SCHEME", "FLAGS")
			for _, s := range out.Suggestions {
				flags := []string{}
				if !s.Online {
					flags = append(flags, "offline")
				}
				if s.Shared {
					flags = append(flags, "shared")
				}
				if s.RequireLogin {
					flags = append(flags, "login")
				}
				name := s.Name
				if name == "" {
					name = "(built-in /ssh)"
				}
				fmt.Printf("%-18s  %-12s  %-18s  %-6s  %s\n", s.Host, s.OsUser, name, s.Scheme, strings.Join(flags, " "))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	return cmd
}

// adminClient wraps a thin HTTP client targeting the local admin UI.
// It transparently attaches the cached session cookie. A 401 surfaces
// as a typed error (errLoginRequired) so callers can print a helpful
// hint instead of a raw "Unauthorized".
type adminClient struct {
	base   string
	cookie string
	http   *http.Client
}

var errLoginRequired = errors.New("not logged in — run `outpost outbound login` first")

// adminBaseURL resolves the admin UI base URL the same way `outpost
// start` does: $OUTPOST_ADMIN_ADDR if set, else the package default.
// We don't import the adminui package directly — a tiny duplicated
// constant beats a cross-binary dependency cycle.
func adminBaseURL() string {
	addr := strings.TrimSpace(os.Getenv("OUTPOST_ADMIN_ADDR"))
	if addr == "" {
		addr = "127.0.0.1:17777"
	}
	return "http://" + addr
}

func newAdminClient(requireCookie bool) (*adminClient, error) {
	c := &adminClient{
		base: adminBaseURL(),
		http: &http.Client{Timeout: 60 * time.Second},
	}
	cookie, err := readAdminCookie()
	if err != nil && requireCookie {
		return nil, errLoginRequired
	}
	c.cookie = cookie
	return c, nil
}

// do is the single point where every outbound request flows through.
// On 401 it returns errLoginRequired regardless of the body so the
// "log in again" hint is consistent across subcommands.
func (a *adminClient) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.cookie != "" {
		req.AddCookie(&http.Cookie{Name: "outpost_admin", Value: a.cookie})
	}
	resp, err := a.http.Do(req)
	if err != nil {
		// Surface a friendlier error for the common case: outpost not
		// running locally. The default net/http message ("connection
		// refused") is correct but unhelpful for a first-time user.
		if strings.Contains(err.Error(), "connection refused") {
			return nil, fmt.Errorf("admin UI not reachable at %s — is outpost running? (`outpost start`)", a.base)
		}
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errLoginRequired
	}
	if resp.StatusCode >= 400 {
		// Try to surface the server's JSON error message; fall back to status.
		var e struct{ Error string }
		if json.Unmarshal(respBody, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("admin UI %d: %s", resp.StatusCode, e.Error)
		}
		return nil, fmt.Errorf("admin UI %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}

// --- subcommands ---

func outboundLoginCmd() *cobra.Command {
	var (
		userFlag  string
		stdinFlag bool
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to the local admin UI (prompts for the LOCAL OS password)",
		Long: `Mints an admin-UI session cookie and caches it at
~/.cache/outpost/admin.cookie. The session TTL is 1 hour; on expiry
or after an outpost restart, run "outpost outbound login" again.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			user := strings.TrimSpace(userFlag)
			if user == "" {
				user, _ = hostauth.CurrentUser()
			}
			if user == "" {
				user = strings.TrimSpace(os.Getenv("USER"))
			}
			if user == "" {
				return errors.New("could not determine OS username; pass --user")
			}
			password, err := readPassword(fmt.Sprintf("Local OS password for %s", user), stdinFlag)
			if err != nil {
				return fmt.Errorf("read password: %w", err)
			}
			if password == "" {
				return errors.New("empty password")
			}
			c, _ := newAdminClient(false)
			// Drive POST /api/login manually so we can capture Set-Cookie
			// off the response — adminClient.do strips response cookies.
			body, _ := json.Marshal(map[string]string{"user": user, "password": password})
			req, _ := http.NewRequestWithContext(cmd.Context(), http.MethodPost,
				c.base+"/api/login", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := c.http.Do(req)
			if err != nil {
				if strings.Contains(err.Error(), "connection refused") {
					return fmt.Errorf("admin UI not reachable at %s — is outpost running? (`outpost start`)", c.base)
				}
				return err
			}
			defer resp.Body.Close()
			rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("login failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(rb)))
			}
			for _, ck := range resp.Cookies() {
				if ck.Name == "outpost_admin" && ck.Value != "" {
					if werr := writeAdminCookie(ck.Value); werr != nil {
						return fmt.Errorf("cache cookie: %w", werr)
					}
					fmt.Fprintf(os.Stderr, "Logged in as %s. Session cached.\n", user)
					return nil
				}
			}
			return errors.New("login accepted but no session cookie returned")
		},
	}
	cmd.Flags().StringVar(&userFlag, "user", "", "OS username to authenticate as (default: $USER)")
	cmd.Flags().BoolVar(&stdinFlag, "stdin", false, "Read password from stdin instead of /dev/tty")
	return cmd
}

func outboundLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Revoke the local admin-UI session and clear the cached cookie",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newAdminClient(true)
			if err != nil {
				// Already logged out — clear the cookie and call it a day.
				_ = removeAdminCookie()
				return nil
			}
			_, _ = c.do(cmd.Context(), http.MethodPost, "/api/logout", nil)
			_ = removeAdminCookie()
			fmt.Fprintln(os.Stderr, "Logged out.")
			return nil
		},
	}
}

func outboundListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured outbound mounts and their connection state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newAdminClient(true)
			if err != nil {
				return err
			}
			body, err := c.do(cmd.Context(), http.MethodGet, "/api/outbound", nil)
			if err != nil {
				return err
			}
			var resp struct {
				Outbound []struct {
					Path        string `json:"path"`
					Name        string `json:"name"`
					Host        string `json:"host"`
					User        string `json:"user"`
					Scheme      string `json:"scheme"`
					LocalPort   int    `json:"local_port"`
					TTLSeconds  int64  `json:"ttl_seconds"`
					Connected   bool   `json:"connected"`
					ConnectedAt string `json:"connected_at"`
				} `json:"outbound"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			if len(resp.Outbound) == 0 {
				fmt.Println("(no outbound mounts)")
				return nil
			}
			// Plain columnar output — keep it simple, no tablewriter dep.
			fmt.Printf("%-20s %-8s %-20s %-12s %-10s %-12s %s\n",
				"PATH", "SCHEME", "TARGET", "USER", "TTL", "STATE", "PORT")
			for _, ob := range resp.Outbound {
				target := ob.Name + "@" + ob.Host
				if ob.Scheme == "ssh" {
					target = "ssh@" + ob.Host
				}
				state := "disconnected"
				if ob.Connected {
					state = "connected"
				}
				port := ""
				if ob.LocalPort != 0 {
					port = strconv.Itoa(ob.LocalPort)
				}
				fmt.Printf("%-20s %-8s %-20s %-12s %-10s %-12s %s\n",
					ob.Path, ob.Scheme, target, ob.User, formatTTL(ob.TTLSeconds), state, port)
			}
			return nil
		},
	}
}

func outboundAddCmd() *cobra.Command {
	var (
		nameFlag      string
		hostFlag      string
		userFlag      string
		schemeFlag    string
		localPortFlag int
		ttlFlag       string
	)
	cmd := &cobra.Command{
		Use:   "add <path>",
		Short: "Register an outbound mount (does NOT connect — run `connect` after)",
		Long: `<path> is the local mount identifier:
  - For scheme=http: it becomes the loopback subpath
    http://127.0.0.1:17777/<path>/ that proxies to the remote app.
  - For scheme=tcp / scheme=ssh: it is just the addressing key;
    the local endpoint is 127.0.0.1:<local-port>.

--ttl overrides cloudbox's absolute-expiry cap on the elevation cookie:
  default | <duration like 24h, 1h30m> | infinite
"infinite" is the JS-safe MaxInt sentinel (~285 years); cloudbox must
be on a version that honors ttl_seconds for it to take effect.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]
			ttl, err := parseTTL(ttlFlag)
			if err != nil {
				return err
			}
			body := map[string]any{
				"path":   path,
				"name":   nameFlag,
				"host":   hostFlag,
				"user":   userFlag,
				"scheme": schemeFlag,
			}
			if localPortFlag > 0 {
				body["local_port"] = localPortFlag
			}
			if ttl != 0 {
				body["ttl_seconds"] = ttl
			}
			c, err := newAdminClient(true)
			if err != nil {
				return err
			}
			if _, err := c.do(cmd.Context(), http.MethodPost, "/api/outbound", body); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Outbound %q registered. Run `outpost outbound connect %s` to activate.\n", path, path)
			return nil
		},
	}
	cmd.Flags().StringVar(&nameFlag, "name", "", "Remote app name (required for http/tcp; ignored for ssh)")
	cmd.Flags().StringVar(&hostFlag, "host", "", "Remote outpost host name (required)")
	cmd.Flags().StringVar(&userFlag, "user", "", "OS username on the remote host (required)")
	cmd.Flags().StringVar(&schemeFlag, "scheme", "http", "http | tcp | ssh")
	cmd.Flags().IntVar(&localPortFlag, "local-port", 0, "Loopback TCP port to bind (required for tcp/ssh)")
	cmd.Flags().StringVar(&ttlFlag, "ttl", "", "Session TTL override: default | <duration> | infinite")
	return cmd
}

func outboundConnectCmd() *cobra.Command {
	var stdinFlag bool
	cmd := &cobra.Command{
		Use:   "connect <path>",
		Short: "Elevate the outbound mount (prompts for the REMOTE host's OS password)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]
			c, err := newAdminClient(true)
			if err != nil {
				return err
			}
			// Discover the remote host name so we can ask for "OS password
			// for <user>@<host>" rather than the opaque path. Cheap GET.
			listBody, err := c.do(cmd.Context(), http.MethodGet, "/api/outbound", nil)
			if err != nil {
				return err
			}
			var listResp struct {
				Outbound []struct {
					Path string `json:"path"`
					Host string `json:"host"`
					User string `json:"user"`
				} `json:"outbound"`
			}
			_ = json.Unmarshal(listBody, &listResp)
			prompt := fmt.Sprintf("Remote OS password for outbound %q", path)
			for _, ob := range listResp.Outbound {
				if ob.Path == path {
					prompt = fmt.Sprintf("Remote OS password for %s@%s", ob.User, ob.Host)
					break
				}
			}
			password, err := readPassword(prompt, stdinFlag)
			if err != nil {
				return fmt.Errorf("read password: %w", err)
			}
			if password == "" {
				return errors.New("empty password")
			}
			_, err = c.do(cmd.Context(), http.MethodPost,
				"/api/outbound/"+path+"/connect",
				map[string]string{"password": password})
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Connected %q.\n", path)
			return nil
		},
	}
	cmd.Flags().BoolVar(&stdinFlag, "stdin", false, "Read password from stdin instead of /dev/tty")
	return cmd
}

func outboundDisconnectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disconnect <path>",
		Short: "Drop the elevation cookie for an outbound mount (config remains)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAdminClient(true)
			if err != nil {
				return err
			}
			path := args[0]
			_, err = c.do(cmd.Context(), http.MethodPost,
				"/api/outbound/"+path+"/disconnect", nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Disconnected %q.\n", path)
			return nil
		},
	}
}

func outboundRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <path>",
		Aliases: []string{"remove", "delete"},
		Short:   "Remove an outbound mount (disconnects first if connected)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAdminClient(true)
			if err != nil {
				return err
			}
			path := args[0]
			_, err = c.do(cmd.Context(), http.MethodDelete, "/api/outbound/"+path, nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Removed %q.\n", path)
			return nil
		},
	}
}

// --- helpers ---

// parseTTL accepts:
//
//	""          → 0   (cloudbox default policy)
//	"default"   → 0
//	"infinite"  → ttlInfiniteSeconds
//	"24h" etc.  → seconds (via time.ParseDuration)
//
// Rejects negative durations, zero durations (use "default" — be
// explicit), and overflows.
func parseTTL(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "default" {
		return 0, nil
	}
	if s == "infinite" || s == "inf" {
		return ttlInfiniteSeconds, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("--ttl %q: expected \"default\", \"infinite\", or a duration like \"24h\"", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("--ttl %q: must be positive (use \"default\" for 0)", s)
	}
	secs := int64(d.Seconds())
	if secs <= 0 || secs > ttlInfiniteSeconds {
		return 0, fmt.Errorf("--ttl %q: out of range", s)
	}
	return secs, nil
}

// formatTTL returns a short human-readable form mirroring the admin UI.
// The "default" / "infinite" labels match what's shown in the SPA so
// users can correlate the CLI output with what they see in the browser.
func formatTTL(secs int64) string {
	if secs <= 0 {
		return "default"
	}
	if secs >= ttlInfiniteSeconds {
		return "infinite"
	}
	d := time.Duration(secs) * time.Second
	// Use Go's built-in duration format for compact display.
	return d.String()
}

// adminCookiePath returns ~/.cache/outpost/admin.cookie, creating the
// parent dir on demand. Mirrors sessionCookiePath in connect.go.
func adminCookiePath() (string, error) {
	dir, err := conf.DefaultCacheDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "admin.cookie"), nil
}

func readAdminCookie() (string, error) {
	path, err := adminCookiePath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", errors.New("empty admin cookie file")
	}
	return v, nil
}

func writeAdminCookie(value string) error {
	path, err := adminCookiePath()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(value); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func removeAdminCookie() error {
	path, err := adminCookiePath()
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
