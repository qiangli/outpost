package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// OutboundManager registers local-mount → remote-outpost-app mappings and
// drives the per-mount connection state.
//
// Lifecycle of one mount:
//
//	  Register      Connect(pw)           Disconnect / pinger-failure
//	cfg only ── elev cookie + pinger ── back to cfg-only
//
// Connect mints a matrix_elev cookie via cloudbox's per-app or
// per-builtin elevate endpoint (Bearer access_token + {user, password}):
//   - http / tcp scheme → POST /h/<host>/app/<name>/elevate
//   - ssh scheme        → POST /h/<host>/ssh/elevate
//
// The cookie's Path narrows to the specific (host, app|builtin), so an
// elevation cannot be replayed against a sibling app. Starts a 4-minute
// pinger to slide the idle TTL. Disconnect (or a pinger failure
// indicating absolute expiry) drops the cookie; the operator must
// Connect again. Cookies are NEVER persisted to disk — only the
// OutboundConfig is.
type OutboundManager struct {
	serverURL   string // base URL of cloudbox HTTP, e.g. https://ai.dhnt.io
	accessToken string

	httpClient *http.Client

	mu      sync.RWMutex
	configs map[string]conf.OutboundConfig // path → cfg (always populated)
	conns   map[string]*outboundConn       // path → live conn (when connected)
}

type outboundConn struct {
	elevCookie  string
	connectedAt time.Time
	cancel      context.CancelFunc
	// listener is set only for tcp-scheme mounts. Disconnect closes it
	// so the accept goroutine exits and pending TCP clients drop.
	listener net.Listener
}

// OutboundView is the API shape the admin UI consumes — config + status.
type OutboundView struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Host        string `json:"host"`
	User        string `json:"user"`
	Scheme      string `json:"scheme"`
	LocalPort   int    `json:"local_port,omitempty"`
	TTLSeconds  int64  `json:"ttl_seconds,omitempty"`
	Connected   bool   `json:"connected"`
	ConnectedAt string `json:"connected_at,omitempty"`
}

// outboundCookieSubdir is the relative path inside conf.DefaultCacheDir() where
// per-mount matrix_elev cookies are persisted. Sibling of the SSH
// session cookie cache (cmd/outpost/connect.go writes to
// <UserCacheDir>/outpost/sessions/) so an operator inspecting cache
// state sees both in one parent dir.
const outboundCookieSubdir = "outbounds"

// cookieCacheDir returns the absolute path of the outbound cookie
// cache directory, creating it (mode 0700) when missing. Errors only
// when UserCacheDir is unresolvable, which would be an OS-level
// configuration issue.
func cookieCacheDir() (string, error) {
	base, err := conf.DefaultCacheDir()
	if err != nil {
		return "", fmt.Errorf("outpost cache dir: %w", err)
	}
	dir := filepath.Join(base, outboundCookieSubdir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// cookieCachePath returns the on-disk cache path for mount `path`'s
// matrix_elev cookie. `path` is operator-supplied (anything cloudbox
// accepts as a mount key); we sanitize to a known charset so a hostile
// name can't escape the cache dir via traversal. Mirrors the
// sanitization in cmd/outpost/connect.go:sessionCookiePath for
// consistency.
func cookieCachePath(path string) (string, error) {
	dir, err := cookieCacheDir()
	if err != nil {
		return "", err
	}
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
	}, path)
	return filepath.Join(dir, safe+".cookie"), nil
}

// writeCookieFile persists a cookie atomically (temp file + rename) so
// a crash mid-write doesn't leave a torn file the next AutoReconnect
// would refuse. Mode 0600: the cookie is bearer-equivalent for the
// (host, app|builtin) scope; readable by anyone with the same uid is
// the right trust boundary.
func writeCookieFile(path, cookie string) error {
	full, err := cookieCachePath(path)
	if err != nil {
		return err
	}
	tmp := full + ".tmp"
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
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, full)
}

// readCookieFile returns the persisted cookie, or "" with no error
// when the file doesn't exist (the common "never connected" case).
// Empty-string return without error lets the caller distinguish
// "no cached cookie" from a real I/O failure.
func readCookieFile(path string) (string, error) {
	full, err := cookieCachePath(path)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// removeCookieFile is the cleanup counterpart of writeCookieFile.
// A missing file is not an error — Disconnect can fire against a
// mount that never persisted a cookie.
func removeCookieFile(path string) error {
	full, err := cookieCachePath(path)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// NewOutboundManager builds a manager with the given cloudbox base URL
// and bearer access_token. serverURL is trimmed of any trailing slash.
// Pass an explicit *http.Client to override the timeout policy in tests;
// nil uses the package default.
func NewOutboundManager(serverURL, accessToken string, client *http.Client) *OutboundManager {
	if client == nil {
		// No global timeout — proxied responses can be streaming (Ollama).
		// The per-request client.Do dance has its own deadlines where
		// needed (the elevate call below uses a wrapped context).
		client = &http.Client{}
	}
	return &OutboundManager{
		serverURL:   strings.TrimRight(serverURL, "/"),
		accessToken: accessToken,
		httpClient:  client,
		configs:     map[string]conf.OutboundConfig{},
		conns:       map[string]*outboundConn{},
	}
}

// Register replaces the registered config set with cfgs. Mounts that
// disappeared get their pinger torn down. A surviving mount keeps its
// live connection only when its cfg is byte-identical to the previous
// one — any change to scheme/local_port/name/host/user invalidates the
// existing conn (in particular, a stale TCP listener on the old port
// must be closed before we'd be willing to bind a new one).
func (m *OutboundManager) Register(cfgs []conf.OutboundConfig) {
	m.mu.Lock()
	next := make(map[string]conf.OutboundConfig, len(cfgs))
	for _, c := range cfgs {
		next[c.Path] = c
	}
	// invalidated collects paths whose elevation became stale either
	// because the cfg row changed or because the mount was removed.
	// Cookies for these get wiped after we drop the lock so disk I/O
	// doesn't block other manager callers.
	var invalidated []string
	for path, conn := range m.conns {
		newCfg, kept := next[path]
		if !kept || newCfg != m.configs[path] {
			conn.cancel()
			if conn.listener != nil {
				_ = conn.listener.Close()
			}
			delete(m.conns, path)
			invalidated = append(invalidated, path)
		}
	}
	// Also wipe cookies for paths that were removed entirely (no live
	// conn to tear down — they may have been in cfg-only state, e.g.
	// after a previous AutoReconnect, before the operator removed the
	// mount via admin UI).
	for path := range m.configs {
		if _, kept := next[path]; !kept {
			// Avoid double-counting paths already in `invalidated`
			// (a removed mount that also had a live conn).
			seen := false
			for _, ip := range invalidated {
				if ip == path {
					seen = true
					break
				}
			}
			if !seen {
				invalidated = append(invalidated, path)
			}
		}
	}
	m.configs = next
	m.mu.Unlock()
	for _, p := range invalidated {
		if rerr := removeCookieFile(p); rerr != nil {
			slog.Warn("outbound: remove stale cookie", "path", p, "err", rerr)
		}
	}
}

// List returns one OutboundView per registered config, sorted by path.
func (m *OutboundManager) List() []OutboundView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]OutboundView, 0, len(m.configs))
	for _, cfg := range m.configs {
		v := OutboundView{
			Path:       cfg.Path,
			Name:       cfg.Name,
			Host:       cfg.Host,
			User:       cfg.User,
			Scheme:     cfg.SchemeNorm(),
			LocalPort:  cfg.LocalPort,
			TTLSeconds: cfg.TTLSeconds,
		}
		if conn, ok := m.conns[cfg.Path]; ok {
			v.Connected = true
			v.ConnectedAt = conn.connectedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Has reports whether path is currently registered.
func (m *OutboundManager) Has(path string) bool {
	m.mu.RLock()
	_, ok := m.configs[path]
	m.mu.RUnlock()
	return ok
}

// Connect authenticates to the remote host via cloudbox's elevate flow
// and stores the resulting matrix_elev cookie in memory. Starts a
// pinger goroutine to slide the idle TTL.
func (m *OutboundManager) Connect(path, password string) error {
	m.mu.RLock()
	cfg, ok := m.configs[path]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown outbound path %q", path)
	}
	if m.serverURL == "" {
		return fmt.Errorf("outbound: cloudbox URL not configured")
	}
	if m.accessToken == "" {
		return fmt.Errorf("outbound: outpost has no access_token — pair with cloudbox first")
	}

	// ttl_seconds is sent only when the operator overrode the default.
	// Cloudbox treats a missing field as "apply the per-host default
	// policy"; math.MaxInt64 means "no absolute cap, only idle expiry".
	// Older cloudbox versions ignore the field harmlessly.
	payload := map[string]any{"user": cfg.User, "password": password}
	if cfg.TTLSeconds != 0 {
		payload["ttl_seconds"] = cfg.TTLSeconds
	}
	body, _ := json.Marshal(payload)
	ctx, cancelReq := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelReq()
	// Elevate URL depends on what we're targeting:
	//   - http/tcp scheme → per-(host, app):     /matrix/h/<host>/elev/app/<name>
	//   - ssh scheme      → per-(host, builtin): /matrix/h/<host>/elev/ssh
	// Cloudbox uses `/elev/` as a literal segment (not a suffix on the
	// data URL) so the per-(host, app) elevate endpoint doesn't collide
	// with the gin catch-all wildcard for HostProxy at
	// /matrix/h/:host/app/:name. The host-wide /matrix/h/<host>/elevate
	// form returns 410.
	elevateURL := m.serverURL + "/matrix/h/" + url.PathEscape(cfg.Host) + "/elev"
	if cfg.BuiltinSSH() {
		elevateURL += "/ssh"
	} else {
		elevateURL += "/app/" + url.PathEscape(cfg.Name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, elevateURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("elevate: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("elevate %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var elev string
	for _, c := range resp.Cookies() {
		if c.Name == "matrix_elev" {
			elev = c.Value
		}
	}
	if elev == "" {
		return fmt.Errorf("elevate succeeded but no matrix_elev cookie returned")
	}

	// For tcp/ssh-scheme mounts: bind the loopback listener BEFORE we
	// publish the conn, so a port-bind failure surfaces synchronously
	// to the caller instead of getting lost in a background goroutine.
	// The listener is loopback-only — same security model as the rest
	// of the agent (only the matrix tunnel is supposed to be addressable
	// off-machine).
	var listener net.Listener
	if cfg.BindsListener() {
		if cfg.LocalPort < 1 || cfg.LocalPort > 65535 {
			return fmt.Errorf("outbound %q: local_port %d is out of range", path, cfg.LocalPort)
		}
		l, lerr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", cfg.LocalPort)))
		if lerr != nil {
			return fmt.Errorf("outbound %q: bind 127.0.0.1:%d: %w", path, cfg.LocalPort, lerr)
		}
		listener = l
	}

	m.mu.Lock()
	if old, ok := m.conns[path]; ok {
		old.cancel()
		if old.listener != nil {
			_ = old.listener.Close()
		}
	}
	pingCtx, cancelPing := context.WithCancel(context.Background())
	conn := &outboundConn{
		elevCookie:  elev,
		connectedAt: time.Now(),
		cancel:      cancelPing,
		listener:    listener,
	}
	m.conns[path] = conn
	m.mu.Unlock()
	// Persist the cookie so a subsequent outpost restart can rehydrate
	// the mount via AutoReconnect without prompting the operator for
	// the OS password again. Best-effort: a write failure is logged
	// but doesn't fail Connect — the live in-memory cookie still
	// works for the current process lifetime, the worst case is the
	// operator has to re-Connect after a restart (the pre-existing
	// behavior, so we're never worse off).
	if werr := writeCookieFile(path, elev); werr != nil {
		slog.Warn("outbound: persist cookie failed", "path", path, "err", werr)
	}
	go m.pinger(pingCtx, path, cfg)
	if listener != nil {
		go m.tcpAcceptLoop(pingCtx, path, cfg, listener)
	}
	return nil
}

// AutoReconnect rehydrates persisted matrix_elev cookies for every
// registered mount and spawns the pinger + (for tcp/ssh-scheme
// mounts) the loopback listener. Equivalent to calling Connect for
// each mount, but without the password / cloudbox round-trip — the
// cookie was minted in a previous outpost lifetime and is good
// until cloudbox's pinger 4xx tears it down.
//
// Call once at startup, AFTER Register. Subsequent Register calls
// don't re-trigger AutoReconnect: those are operator-initiated
// config edits where the right semantic is "respect the new state
// exactly" (and Register already wipes stale cookies for changed
// rows).
//
// Listener bind failures (port already in use, permission denied)
// are logged per-mount and the affected mount is left in cfg-only
// state; the operator can resolve the conflict and Connect
// manually. Successful mounts are not blocked by a failure on a
// sibling — each mount is independent.
func (m *OutboundManager) AutoReconnect() {
	if m.serverURL == "" || m.accessToken == "" {
		// Unpaired outpost: nothing to ping against. Same posture
		// as Connect, which already guards on these.
		return
	}
	m.mu.RLock()
	cfgs := make([]conf.OutboundConfig, 0, len(m.configs))
	for _, cfg := range m.configs {
		cfgs = append(cfgs, cfg)
	}
	m.mu.RUnlock()

	for _, cfg := range cfgs {
		cookie, err := readCookieFile(cfg.Path)
		if err != nil {
			slog.Warn("outbound: read persisted cookie", "path", cfg.Path, "err", err)
			continue
		}
		if cookie == "" {
			// No cached cookie for this mount — operator never
			// Connected it, or Disconnect wiped it. Skip silently;
			// this is the unpaired-mount default state.
			continue
		}
		if err := m.hydrateMount(cfg, cookie); err != nil {
			// Already in conns from a prior call, or a listener
			// bind conflict, or some other one-mount error. Log,
			// move on, don't wipe the cookie (a transient port
			// conflict shouldn't force the operator to re-elevate).
			slog.Warn("outbound: rehydrate failed", "path", cfg.Path, "err", err)
		}
	}
}

// hydrateMount installs an outboundConn into m.conns for cfg using a
// known-valid cookie and spawns the pinger + listener. Returns an
// error if the mount is already connected (Connect would do this
// instead) or if the listener bind fails. Shared by AutoReconnect
// and any future "reconnect-on-cookie-refresh" path.
//
// Caller must NOT hold m.mu — this method takes the write lock.
func (m *OutboundManager) hydrateMount(cfg conf.OutboundConfig, cookie string) error {
	if strings.TrimSpace(cookie) == "" {
		return fmt.Errorf("hydrateMount %q: empty cookie", cfg.Path)
	}
	var listener net.Listener
	if cfg.BindsListener() {
		if cfg.LocalPort < 1 || cfg.LocalPort > 65535 {
			return fmt.Errorf("local_port %d is out of range", cfg.LocalPort)
		}
		l, lerr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", cfg.LocalPort)))
		if lerr != nil {
			return fmt.Errorf("bind 127.0.0.1:%d: %w", cfg.LocalPort, lerr)
		}
		listener = l
	}

	m.mu.Lock()
	if _, already := m.conns[cfg.Path]; already {
		m.mu.Unlock()
		if listener != nil {
			_ = listener.Close()
		}
		return fmt.Errorf("already connected")
	}
	pingCtx, cancelPing := context.WithCancel(context.Background())
	m.conns[cfg.Path] = &outboundConn{
		elevCookie:  cookie,
		connectedAt: time.Now(),
		cancel:      cancelPing,
		listener:    listener,
	}
	m.mu.Unlock()
	go m.pinger(pingCtx, cfg.Path, cfg)
	if listener != nil {
		go m.tcpAcceptLoop(pingCtx, cfg.Path, cfg, listener)
	}
	slog.Info("outbound: rehydrated from persisted cookie", "path", cfg.Path, "scheme", cfg.SchemeNorm())
	return nil
}

// Disconnect drops the in-memory cookie for path, stops the pinger,
// removes the persisted cookie file, and — for tcp mounts — closes
// the loopback listener. No server-side revocation: cloudbox's
// matrix_elev is a stateless JWT.
func (m *OutboundManager) Disconnect(path string) {
	m.mu.Lock()
	if conn, ok := m.conns[path]; ok {
		conn.cancel()
		if conn.listener != nil {
			_ = conn.listener.Close()
		}
		delete(m.conns, path)
	}
	m.mu.Unlock()
	// Remove the persisted cookie file outside the lock so a slow
	// filesystem doesn't block other Connect/Disconnect callers.
	// Best-effort: a stale file just means a stale cookie that the
	// next AutoReconnect would harmlessly try (and the pinger would
	// 4xx, triggering the same self-clean).
	if rerr := removeCookieFile(path); rerr != nil {
		slog.Warn("outbound: remove persisted cookie failed", "path", path, "err", rerr)
	}
}

// pinger keeps the matrix_elev cookie alive by POSTing /elevate-ping
// every 4 minutes. The cookie is idle-expired by cloudbox after 5 min;
// pinging twice within that window keeps it warm. Hard absolute expiry
// (1 h) is observable as a non-2xx response, which tears the conn down
// — the operator must Connect again. The ping URL has to match the
// cookie's Path or cloudbox won't see the cookie: per-(host, app) for
// http/tcp scheme, host-level for ssh scheme.
func (m *OutboundManager) pinger(ctx context.Context, path string, cfg conf.OutboundConfig) {
	pingURL := m.serverURL + "/matrix/h/" + url.PathEscape(cfg.Host) + "/elev"
	if cfg.BuiltinSSH() {
		pingURL += "/ssh/ping"
	} else {
		pingURL += "/app/" + url.PathEscape(cfg.Name) + "/ping"
	}
	t := time.NewTicker(4 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.mu.RLock()
		conn := m.conns[path]
		m.mu.RUnlock()
		if conn == nil {
			return
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, pingURL, nil)
		req.Header.Set("Authorization", "Bearer "+m.accessToken)
		req.AddCookie(&http.Cookie{Name: "matrix_elev", Value: conn.elevCookie})
		resp, err := m.httpClient.Do(req)
		if err != nil {
			slog.Warn("outbound pinger transient error", "path", path, "err", err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 400 {
			slog.Warn("outbound elevation rejected — disconnecting", "path", path, "status", resp.StatusCode)
			m.Disconnect(path)
			return
		}
	}
}

// ProxyTo is the request handler. It forwards an inbound HTTP request
// (already stripped of the leading /<path>/ prefix into `rest`) through
// cloudbox to the remote outpost's registered app. Streaming responses
// (Ollama's /api/generate, etc.) flow through because we copy resp.Body
// to the gin writer with io.Copy and never buffer the full body.
func (m *OutboundManager) ProxyTo(c *gin.Context, path, rest string) {
	m.mu.RLock()
	cfg, ok := m.configs[path]
	conn := m.conns[path]
	m.mu.RUnlock()
	if !ok {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "unknown outbound path"})
		return
	}
	if cfg.BindsListener() {
		// HTTP requests to a tcp/ssh-scheme outbound are a category
		// error — those mounts expose a 127.0.0.1:<local_port> TCP
		// listener, not an admin-UI subpath.
		c.AbortWithStatusJSON(http.StatusBadRequest,
			gin.H{"error": fmt.Sprintf("outbound %q is %s — connect to 127.0.0.1:%d, not the admin UI", path, cfg.SchemeNorm(), cfg.LocalPort)})
		return
	}
	if conn == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable,
			gin.H{"error": "outbound not connected — click Connect in the admin UI"})
		return
	}
	if rest == "" {
		rest = "/"
	}
	upstream := m.serverURL +
		"/matrix/h/" + url.PathEscape(cfg.Host) +
		"/app/" + url.PathEscape(cfg.Name) +
		rest
	if c.Request.URL.RawQuery != "" {
		upstream += "?" + c.Request.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, upstream, c.Request.Body)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	// Mirror most incoming headers, but never forward Cookies or Host —
	// those are tied to the local admin-UI listener, not cloudbox.
	for k, v := range c.Request.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Host", "Cookie", "Authorization", "Content-Length":
			continue
		}
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+m.accessToken)
	req.AddCookie(&http.Cookie{Name: "matrix_elev", Value: conn.elevCookie})

	resp, err := m.httpClient.Do(req)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		// Drop hop-by-hop headers and the Set-Cookie that would leak
		// cloudbox cookies back to the local browser.
		switch http.CanonicalHeaderKey(k) {
		case "Set-Cookie", "Transfer-Encoding", "Connection", "Keep-Alive":
			continue
		}
		c.Writer.Header()[k] = v
	}
	c.Writer.WriteHeader(resp.StatusCode)
	// Streaming-friendly copy. ResponseWriter flushes on each Write for
	// chunked responses; net/http handles that for us.
	_, _ = io.Copy(c.Writer, resp.Body)
}

// Stop cancels every pinger and closes any tcp listeners. Call on
// process shutdown so goroutines exit cleanly. Configs and the
// in-memory cookie map are NOT cleared; another process boot will
// reload configs from disk (cookies are not persisted).
func (m *OutboundManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for path, conn := range m.conns {
		conn.cancel()
		if conn.listener != nil {
			_ = conn.listener.Close()
		}
		delete(m.conns, path)
	}
}

// tcpAcceptLoop owns the listener for one tcp/ssh-scheme outbound.
// Each accepted client connection spawns a goroutine that opens a WSS
// to cloudbox — at /h/<host>/app/<name>/ for tcp or /h/<host>/ssh for
// ssh — and byte-splices the two ends. When ctx is cancelled
// (Disconnect / Stop / Register-removal) the listener Close races us
// out of Accept.
func (m *OutboundManager) tcpAcceptLoop(ctx context.Context, path string, cfg conf.OutboundConfig, l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Warn("outbound tcp accept", "path", path, "err", err)
			return
		}
		go m.bridgeTCP(ctx, path, cfg, conn)
	}
}

// bridgeTCP carries a single local TCP connection over the WSS tunnel
// to either the remote outpost's tcp app (scheme="tcp") or its built-in
// /ssh WebSocket SSH server (scheme="ssh"). The matrix_elev cookie that
// gates access is captured at Connect time and replayed here per-conn.
func (m *OutboundManager) bridgeTCP(ctx context.Context, path string, cfg conf.OutboundConfig, client net.Conn) {
	defer client.Close()

	// Re-read the cookie at dial time so we always use the latest one
	// (the pinger refreshes the conn record on disconnect-on-failure).
	m.mu.RLock()
	state := m.conns[path]
	m.mu.RUnlock()
	if state == nil {
		return
	}
	cookie := state.elevCookie

	wsURL := strings.Replace(m.serverURL, "http", "ws", 1) + "/matrix/h/" + url.PathEscape(cfg.Host)
	if cfg.BuiltinSSH() {
		wsURL += "/ssh"
	} else {
		wsURL += "/app/" + url.PathEscape(cfg.Name) + "/"
	}

	dialCtx, cancelDial := context.WithTimeout(ctx, 15*time.Second)
	defer cancelDial()
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+m.accessToken)
	headers.Set("Cookie", (&http.Cookie{Name: "matrix_elev", Value: cookie}).String())
	ws, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPClient: m.httpClient,
		HTTPHeader: headers,
	})
	if err != nil {
		slog.Warn("outbound tcp ws dial", "path", path, "err", err)
		return
	}
	defer ws.Close(websocket.StatusInternalError, "closing")

	bridgeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wsConn := websocket.NetConn(bridgeCtx, ws, websocket.MessageBinary)
	defer wsConn.Close()

	go func() {
		defer cancel()
		_, _ = io.Copy(wsConn, client)
	}()
	_, _ = io.Copy(client, wsConn)
}
