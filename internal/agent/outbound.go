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
// Connect calls cloudbox's POST /h/<host>/elevate with Bearer
// access_token + {user, password}, captures the matrix_elev cookie, and
// starts a 4-minute pinger to slide the idle TTL. Disconnect (or a
// pinger failure indicating absolute expiry) drops the cookie; the
// operator must Connect again. Cookies are NEVER persisted to disk —
// only the OutboundConfig is.
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
	Connected   bool   `json:"connected"`
	ConnectedAt string `json:"connected_at,omitempty"`
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
	defer m.mu.Unlock()
	next := make(map[string]conf.OutboundConfig, len(cfgs))
	for _, c := range cfgs {
		next[c.Path] = c
	}
	for path, conn := range m.conns {
		newCfg, kept := next[path]
		if !kept || newCfg != m.configs[path] {
			conn.cancel()
			if conn.listener != nil {
				_ = conn.listener.Close()
			}
			delete(m.conns, path)
		}
	}
	m.configs = next
}

// List returns one OutboundView per registered config, sorted by path.
func (m *OutboundManager) List() []OutboundView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]OutboundView, 0, len(m.configs))
	for _, cfg := range m.configs {
		v := OutboundView{
			Path:      cfg.Path,
			Name:      cfg.Name,
			Host:      cfg.Host,
			User:      cfg.User,
			Scheme:    cfg.SchemeNorm(),
			LocalPort: cfg.LocalPort,
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

	body, _ := json.Marshal(map[string]string{"user": cfg.User, "password": password})
	ctx, cancelReq := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelReq()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.serverURL+"/h/"+url.PathEscape(cfg.Host)+"/elevate", bytes.NewReader(body))
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

	// For tcp-scheme mounts: bind the loopback listener BEFORE we publish
	// the conn, so a port-bind failure surfaces synchronously to the
	// caller instead of getting lost in a background goroutine. The
	// listener is loopback-only — same security model as the rest of
	// the agent (only the matrix tunnel is supposed to be addressable
	// off-machine).
	var listener net.Listener
	if cfg.SchemeNorm() == "tcp" {
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
	host := cfg.Host
	m.mu.Unlock()
	go m.pinger(pingCtx, path, host)
	if listener != nil {
		go m.tcpAcceptLoop(pingCtx, path, cfg, listener)
	}
	return nil
}

// Disconnect drops the in-memory cookie for path, stops the pinger, and
// — for tcp mounts — closes the loopback listener. No server-side
// revocation: cloudbox's matrix_elev is a stateless JWT.
func (m *OutboundManager) Disconnect(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if conn, ok := m.conns[path]; ok {
		conn.cancel()
		if conn.listener != nil {
			_ = conn.listener.Close()
		}
		delete(m.conns, path)
	}
}

// pinger keeps the matrix_elev cookie alive by POSTing /elevate-ping
// every 4 minutes. The cookie is idle-expired by cloudbox after 5 min;
// pinging twice within that window keeps it warm. Hard absolute expiry
// (1 h) is observable as a non-2xx response, which tears the conn down
// — the operator must Connect again.
func (m *OutboundManager) pinger(ctx context.Context, path, host string) {
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
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			m.serverURL+"/h/"+url.PathEscape(host)+"/elevate-ping", nil)
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
	if cfg.SchemeNorm() == "tcp" {
		// HTTP requests to a tcp-scheme outbound are a category error —
		// the local mount for a tcp outbound is a 127.0.0.1:<local_port>
		// TCP listener, not an admin-UI subpath.
		c.AbortWithStatusJSON(http.StatusBadRequest,
			gin.H{"error": fmt.Sprintf("outbound %q is tcp — connect to 127.0.0.1:%d, not the admin UI", path, cfg.LocalPort)})
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
		"/h/" + url.PathEscape(cfg.Host) +
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

// tcpAcceptLoop owns the listener for one tcp-scheme outbound. Each
// accepted client connection spawns a goroutine that opens a WSS to
// cloudbox at /h/<host>/app/<name>/ and byte-splices the two ends.
// When ctx is cancelled (Disconnect / Stop / Register-removal) the
// listener Close races us out of Accept.
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
// to the remote outpost's tcp app. The matrix_elev cookie that gates
// access is captured at Connect time and replayed here per-conn.
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

	wsURL := strings.Replace(m.serverURL, "http", "ws", 1) +
		"/h/" + url.PathEscape(cfg.Host) +
		"/app/" + url.PathEscape(cfg.Name) + "/"

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
