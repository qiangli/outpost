package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// AppEntry is one declared app's name + minimum clearance, as published
// to the cloud via GET /apps. Role values match the cloud's vocabulary:
// guest | user | admin. Empty role defaults to "user".
type AppEntry struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

// AppRegistry maps app names (e.g. "ycode") to the local URL they live at
// (e.g. "http://127.0.0.1:8765"). It is loopback-only by design: the agent
// itself is only reachable through the matrix tunnel, and the agent only
// forwards under tier-1 trust (set by the cloud server in the
// X-Periscope-User header).
//
// Socket-backed entries (scheme=unix|npipe) use a per-app Transport whose
// DialContext dials the local socket; the URL host is a synthetic "socket"
// placeholder that the upstream daemon ignores.
type AppRegistry struct {
	mu    sync.RWMutex
	apps  map[string]*url.URL
	proxy map[string]*httputil.ReverseProxy
	roles map[string]string
	// tcp holds the raw host:port destination for tcp-scheme apps. Its
	// key set is disjoint from `proxy` — TCP apps don't speak HTTP, so
	// they get a dedicated WS↔TCP bridge handler rather than the
	// ReverseProxy path.
	tcp map[string]string
}

func NewAppRegistry() *AppRegistry {
	return &AppRegistry{
		apps:  map[string]*url.URL{},
		proxy: map[string]*httputil.ReverseProxy{},
		roles: map[string]string{},
		tcp:   map[string]string{},
	}
}

// Register adds (or replaces) an app entry. target must be an absolute URL.
// Role defaults to "user" — use RegisterWithRole to set explicitly.
func (r *AppRegistry) Register(name, target string) error {
	return r.RegisterWithRole(name, target, "")
}

// RegisterWithRole is Register that also records the minimum clearance.
// Empty role defaults to "user". Roles outside {guest, user, admin} are
// rejected.
func (r *AppRegistry) RegisterWithRole(name, target, role string) error {
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("app %q target: %w", name, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("app %q target must be absolute (got %q)", name, target)
	}
	return r.register(name, u, role, nil)
}

// register is the unified internal entry point. target must have Scheme
// "http" or "https". For socket-backed apps, callers pass a synthetic
// http://socket URL plus a transport whose DialContext dials the socket.
// role "" defaults to "user". Roles outside {guest, user, admin} are rejected.
func (r *AppRegistry) register(name string, target *url.URL, role string, transport http.RoundTripper) error {
	if !conf.ValidRole(role) {
		return fmt.Errorf("app %q role %q: must be one of guest|user|admin", name, role)
	}
	if role == "" {
		role = "user"
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	if transport != nil {
		rp.Transport = transport
	}
	r.mu.Lock()
	r.apps[name] = target
	r.proxy[name] = rp
	r.roles[name] = role
	// Mode swap: an HTTP register wins over any prior tcp entry with the
	// same name. (Re-registers in the other direction do the inverse in
	// registerTCP.)
	delete(r.tcp, name)
	r.mu.Unlock()
	return nil
}

// Names returns registered app names (HTTP and TCP combined).
func (r *AppRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.apps)+len(r.tcp))
	for k := range r.apps {
		out = append(out, k)
	}
	for k := range r.tcp {
		out = append(out, k)
	}
	return out
}

// Entries returns the registered apps with their declared roles. Used by
// GET /apps so the cloud sees per-app role declarations and can gate
// custom apps accurately. TCP apps are listed alongside HTTP ones; the
// cloud doesn't need to know the difference, since transport selection
// happens entirely inside the agent's /app/<name>/ handler.
func (r *AppRegistry) Entries() []AppEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AppEntry, 0, len(r.apps)+len(r.tcp))
	for name := range r.apps {
		role := r.roles[name]
		if role == "" {
			role = "user"
		}
		out = append(out, AppEntry{Name: name, Role: role})
	}
	for name := range r.tcp {
		role := r.roles[name]
		if role == "" {
			role = "user"
		}
		out = append(out, AppEntry{Name: name, Role: role})
	}
	return out
}

// LookupTarget returns the registered HTTP target URL (or nil). TCP-mode
// apps have no URL; use LookupTCP for those.
func (r *AppRegistry) LookupTarget(name string) *url.URL {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.apps[name]
}

// LookupTCP returns the registered TCP target "host:port" (or "" when
// name is not a TCP-mode app).
func (r *AppRegistry) LookupTCP(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tcp[name]
}

// Unregister removes an app entry. No-op if the name is not registered.
func (r *AppRegistry) Unregister(name string) {
	r.mu.Lock()
	delete(r.apps, name)
	delete(r.proxy, name)
	delete(r.roles, name)
	delete(r.tcp, name)
	r.mu.Unlock()
}

// RegisterFromConfig is a convenience that builds a target from an
// AppConfig and registers it. Disabled entries are skipped (so the admin
// UI can keep them around without proxying them). Socket-backed apps
// (scheme=unix|npipe) get a custom Transport that dials the socket.
func (r *AppRegistry) RegisterFromConfig(ac conf.AppConfig) error {
	if !ac.Enabled {
		return nil
	}
	scheme := strings.ToLower(strings.TrimSpace(ac.Scheme))
	if scheme == "" {
		scheme = "http"
	}
	switch scheme {
	case "http", "https":
		host := strings.TrimSpace(ac.Host)
		if host == "" {
			host = "127.0.0.1"
		}
		if ac.Port <= 0 || ac.Port > 65535 {
			return fmt.Errorf("app %q: port %d is out of range", ac.Name, ac.Port)
		}
		u := &url.URL{Scheme: scheme, Host: host + ":" + strconv.Itoa(ac.Port)}
		return r.register(ac.Name, u, ac.Role, nil)
	case "unix", "npipe":
		sock := strings.TrimSpace(ac.Socket)
		if sock == "" {
			return fmt.Errorf("app %q: socket path is required for scheme %q", ac.Name, scheme)
		}
		// Synthetic http://socket; the real connection is dialed by the
		// per-app transport's DialContext. The upstream daemon (podman/
		// docker/ollama) ignores the Host header.
		u := &url.URL{Scheme: "http", Host: "socket"}
		return r.register(ac.Name, u, ac.Role, socketTransport(scheme, sock))
	case "tcp":
		host := strings.TrimSpace(ac.Host)
		if host == "" {
			host = "127.0.0.1"
		}
		if ac.Port <= 0 || ac.Port > 65535 {
			return fmt.Errorf("app %q: port %d is out of range", ac.Name, ac.Port)
		}
		return r.registerTCP(ac.Name, net.JoinHostPort(host, strconv.Itoa(ac.Port)), ac.Role)
	default:
		return fmt.Errorf("app %q: scheme must be one of http|https|tcp|unix|npipe (got %q)", ac.Name, ac.Scheme)
	}
}

// registerTCP records name → host:port for a tcp-scheme app. The
// /app/<name>/ handler will accept a WebSocket upgrade and byte-bridge
// to that address. Disjoint from the HTTP register: a name is either
// HTTP-mode or TCP-mode, not both.
func (r *AppRegistry) registerTCP(name, addr, role string) error {
	if !conf.ValidRole(role) {
		return fmt.Errorf("app %q role %q: must be one of guest|user|admin", name, role)
	}
	if role == "" {
		role = "user"
	}
	r.mu.Lock()
	// Same name can't be both HTTP- and TCP-mode at once.
	delete(r.apps, name)
	delete(r.proxy, name)
	r.tcp[name] = addr
	r.roles[name] = role
	r.mu.Unlock()
	return nil
}

// socketTransport returns an http.Transport whose DialContext routes every
// connection to a local socket regardless of the request URL's host.
// Suitable for fronting docker.sock / podman.sock / Windows named pipes.
// HTTP/1.1 Upgrade (used by podman's /attach and /exec) and websockets
// continue to work because httputil.ReverseProxy hijacks the conn from
// this transport's response — the same code path as the default transport.
func socketTransport(scheme, socket string) http.RoundTripper {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialSocket(ctx, scheme, socket)
		},
		MaxIdleConns:       4,
		IdleConnTimeout:    30 * time.Second,
		DisableCompression: true,
	}
}

// handler returns a gin handler that proxies `/app/:name/*p` to the
// registered app's local URL, stripping the `/app/:name` prefix so the
// app sees its native paths.
func (r *AppRegistry) handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		r.ProxyTo(c, c.Param("name"), c.Param("p"))
	}
}

// ProxyTo is the gin-param-free entry point used by both the standard
// /app/:name/*p route and the admin UI's `/<name>/*` local-access route
// (so users can hit http://localhost:17777/ollama/... directly without
// going through the cloudbox tunnel). Callers pass the captured wildcard
// `rest` as the upstream path to forward (leading slash included; an
// empty value is treated as "/").
//
// For TCP-mode apps (ssh, postgres, …) the same route accepts a
// WebSocket upgrade and byte-bridges to the registered host:port. The
// `rest` argument is ignored — TCP has no notion of a sub-path.
func (r *AppRegistry) ProxyTo(c *gin.Context, name, rest string) {
	r.mu.RLock()
	rp := r.proxy[name]
	target := r.apps[name]
	tcpAddr := r.tcp[name]
	r.mu.RUnlock()
	if tcpAddr != "" {
		serveTCPBridge(c, name, tcpAddr)
		return
	}
	if rp == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "unknown app: " + name})
		return
	}
	if rest == "" {
		rest = "/"
	}
	c.Request.URL.Path = singleJoin(target.Path, rest)
	c.Request.URL.RawPath = ""
	rp.ServeHTTP(c.Writer, c.Request)
}

// serveTCPBridge accepts the inbound WebSocket and byte-splices it onto
// a freshly-dialed TCP conn to addr. Both ends are closed when either
// side EOFs or the request context cancels.
//
// Trust note: this handler does no auth of its own — same as /shell and
// /desktop, it relies on cloudbox's elev-cookie gate to fence access at
// the edge. The agent listens loopback-only; the only ingress is the
// matrix tunnel.
func serveTCPBridge(c *gin.Context, name, addr string) {
	ws, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Warn("tcp-app ws accept", "app", name, "err", err)
		return
	}
	defer ws.Close(websocket.StatusInternalError, "closing")

	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	dialer := net.Dialer{Timeout: 5 * time.Second}
	upstream, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		slog.Warn("tcp-app dial", "app", name, "addr", addr, "err", err)
		_ = ws.Close(websocket.StatusBadGateway, "dial upstream failed")
		return
	}
	defer upstream.Close()

	wsConn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	defer wsConn.Close()

	// Two copies in opposite directions; either return triggers cancel,
	// which causes the other side to unblock and return too.
	go func() {
		defer cancel()
		_, _ = io.Copy(upstream, wsConn)
	}()
	_, _ = io.Copy(wsConn, upstream)
}

func singleJoin(a, b string) string {
	switch {
	case a == "" || a == "/":
		return b
	case strings.HasSuffix(a, "/") && strings.HasPrefix(b, "/"):
		return a + b[1:]
	case !strings.HasSuffix(a, "/") && !strings.HasPrefix(b, "/"):
		return a + "/" + b
	default:
		return a + b
	}
}
