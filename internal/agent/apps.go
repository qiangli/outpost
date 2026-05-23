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

// AppEntry is one declared app published to the cloud via GET /apps.
//
//   - RequireLogin: when true, outpost (and cloudbox at the edge)
//     require the caller to have proven local-OS auth before this
//     app's tile/proxy is reachable. Replaces the legacy guest/user/
//     admin role tier.
//   - Scheme: "http" for the reverse-proxy path or "tcp" for the
//     WS↔TCP bridge; cloudbox uses it to know whether the local-side
//     mount needs a subpath (http) or a TCP listener port (tcp).
//   - IndexPath: optional landing sub-path the cloudbox SPA prepends
//     when constructing this app's tile URL. Empty = "/". Enables the
//     "virtual app" pattern (two AppConfig rows on the same upstream
//     opening at different paths) without any proxy-side rewriting.
type AppEntry struct {
	Name         string `json:"name"`
	Scheme       string `json:"scheme,omitempty"`
	RequireLogin bool   `json:"require_login"`
	IndexPath    string `json:"index_path,omitempty"`
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
	// Per-app access-control + display metadata. All keyed by app
	// name; entries kept in lockstep with `apps`/`proxy`/`tcp` by
	// register/registerTCP/Unregister.
	requireLogin map[string]bool
	lanOnly      map[string][]string
	indexPath    map[string]string
	// tcp holds the raw host:port destination for tcp-scheme apps. Its
	// key set is disjoint from `proxy` — TCP apps don't speak HTTP, so
	// they get a dedicated WS↔TCP bridge handler rather than the
	// ReverseProxy path.
	tcp map[string]string
}

func NewAppRegistry() *AppRegistry {
	return &AppRegistry{
		apps:         map[string]*url.URL{},
		proxy:        map[string]*httputil.ReverseProxy{},
		requireLogin: map[string]bool{},
		lanOnly:      map[string][]string{},
		indexPath:    map[string]string{},
		tcp:          map[string]string{},
	}
}

// AppMeta carries the access-control + display fields the registry
// associates with each app. Internal to register/registerTCP. Tests
// and the simple Register helper pass a zero-value or a partial value.
type AppMeta struct {
	RequireLogin bool
	LANOnlyPaths []string
	IndexPath    string
}

// Register adds (or replaces) an app entry with the default
// access-control posture (RequireLogin=true, no LAN-only paths, no
// IndexPath). target must be an absolute URL. Used by tests and by
// callers that don't have an AppConfig handy.
func (r *AppRegistry) Register(name, target string) error {
	return r.RegisterWithMeta(name, target, AppMeta{RequireLogin: true})
}

// RegisterWithMeta is Register with explicit per-app metadata. Used
// by built-in apps and tests that need to assert specific gating.
func (r *AppRegistry) RegisterWithMeta(name, target string, meta AppMeta) error {
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("app %q target: %w", name, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("app %q target must be absolute (got %q)", name, target)
	}
	return r.register(name, u, meta, nil)
}

// register is the unified internal entry point. target must have Scheme
// "http" or "https". For socket-backed apps, callers pass a synthetic
// http://socket URL plus a transport whose DialContext dials the socket.
// role "" defaults to "user". Roles outside {guest, user, admin} are rejected.
//
// The Rewrite callback sets X-Forwarded-* headers so well-behaved
// upstream web apps (Grafana, JupyterLab, code-server, …) can compute
// correct absolute URLs without needing per-app rewrite rules on
// outpost. Cloudbox-supplied X-Forwarded-* values flow through
// unchanged via the request clone; outpost only fills in defaults when
// a header is missing (the common case for direct loopback access).
func (r *AppRegistry) register(name string, target *url.URL, meta AppMeta, transport http.RoundTripper) error {
	rp := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// httputil.ReverseProxy strips the four standard X-Forwarded-*
			// headers from pr.Out before calling Rewrite (anti-smuggling
			// for the case where outpost is reached directly by an
			// untrusted client). We read pr.In — where cloudbox's values
			// still live — and selectively re-emit them on pr.Out so the
			// upstream app can build correct absolute URLs.
			inHost := pr.In.Header.Get("X-Forwarded-Host")
			if inHost == "" {
				inHost = pr.In.Host
			}
			if inHost != "" {
				pr.Out.Header.Set("X-Forwarded-Host", inHost)
			}
			inProto := pr.In.Header.Get("X-Forwarded-Proto")
			if inProto == "" {
				inProto = "http"
				if pr.In.TLS != nil {
					inProto = "https"
				}
			}
			pr.Out.Header.Set("X-Forwarded-Proto", inProto)
			// X-Forwarded-Prefix lets the app know its public mount
			// point. Cloudbox should set it to the full external prefix
			// (e.g. /h/<host>/app/<name>); we fall back to /app/<name>
			// so the loopback admin UI still produces a usable value.
			inPrefix := pr.In.Header.Get("X-Forwarded-Prefix")
			if inPrefix == "" {
				inPrefix = "/app/" + name
			}
			pr.Out.Header.Set("X-Forwarded-Prefix", inPrefix)
			// X-Forwarded-For: append the immediate client IP to any
			// existing chain from pr.In.
			if clientIP, _, err := net.SplitHostPort(pr.In.RemoteAddr); err == nil {
				prior := pr.In.Header.Values("X-Forwarded-For")
				if len(prior) > 0 {
					clientIP = strings.Join(prior, ", ") + ", " + clientIP
				}
				pr.Out.Header.Set("X-Forwarded-For", clientIP)
			}
		},
	}
	r.mu.Lock()
	r.apps[name] = target
	r.proxy[name] = rp
	r.requireLogin[name] = meta.RequireLogin
	r.lanOnly[name] = append([]string(nil), meta.LANOnlyPaths...)
	r.indexPath[name] = meta.IndexPath
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

// Entries returns the registered apps' metadata for the GET /apps
// publish. TCP apps are listed alongside HTTP ones; the cloud uses
// `scheme` to pick the right tile shape.
func (r *AppRegistry) Entries() []AppEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AppEntry, 0, len(r.apps)+len(r.tcp))
	for name, u := range r.apps {
		// u.Scheme is "http" for both http/https/unix/npipe (the synthetic
		// http://socket URL stores "http" for socket-backed apps). That's
		// the right value for the cloud — schemes are transport hints, not
		// the on-disk AppConfig.Scheme. Socket apps proxy HTTP, period.
		scheme := u.Scheme
		if scheme == "" {
			scheme = "http"
		}
		out = append(out, AppEntry{
			Name:         name,
			Scheme:       scheme,
			RequireLogin: r.requireLogin[name],
			IndexPath:    r.indexPath[name],
		})
	}
	for name := range r.tcp {
		out = append(out, AppEntry{
			Name:         name,
			Scheme:       "tcp",
			RequireLogin: r.requireLogin[name],
			IndexPath:    r.indexPath[name],
		})
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
	delete(r.requireLogin, name)
	delete(r.lanOnly, name)
	delete(r.indexPath, name)
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
	meta := AppMeta{
		RequireLogin: ac.RequireLogin,
		LANOnlyPaths: ac.LANOnlyPaths,
		IndexPath:    ac.IndexPath,
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
		return r.register(ac.Name, u, meta, nil)
	case "unix", "npipe":
		sock := strings.TrimSpace(ac.Socket)
		if sock == "" {
			return fmt.Errorf("app %q: socket path is required for scheme %q", ac.Name, scheme)
		}
		// Synthetic http://socket; the real connection is dialed by the
		// per-app transport's DialContext. The upstream daemon (podman/
		// docker/ollama) ignores the Host header.
		u := &url.URL{Scheme: "http", Host: "socket"}
		return r.register(ac.Name, u, meta, socketTransport(scheme, sock))
	case "tcp":
		host := strings.TrimSpace(ac.Host)
		if host == "" {
			host = "127.0.0.1"
		}
		if ac.Port <= 0 || ac.Port > 65535 {
			return fmt.Errorf("app %q: port %d is out of range", ac.Name, ac.Port)
		}
		return r.registerTCP(ac.Name, net.JoinHostPort(host, strconv.Itoa(ac.Port)), meta)
	default:
		return fmt.Errorf("app %q: scheme must be one of http|https|tcp|unix|npipe (got %q)", ac.Name, ac.Scheme)
	}
}

// registerTCP records name → host:port for a tcp-scheme app. The
// /app/<name>/ handler will accept a WebSocket upgrade and byte-bridge
// to that address. Disjoint from the HTTP register: a name is either
// HTTP-mode or TCP-mode, not both. LANOnlyPaths is meaningless for tcp
// (no path concept) but we store the value verbatim so an HTTP↔TCP
// mode swap on the same name keeps the operator's declarations intact.
func (r *AppRegistry) registerTCP(name, addr string, meta AppMeta) error {
	r.mu.Lock()
	// Same name can't be both HTTP- and TCP-mode at once.
	delete(r.apps, name)
	delete(r.proxy, name)
	r.tcp[name] = addr
	r.requireLogin[name] = meta.RequireLogin
	r.lanOnly[name] = append([]string(nil), meta.LANOnlyPaths...)
	r.indexPath[name] = meta.IndexPath
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
	tcpAddr := r.tcp[name]
	requireLogin := r.requireLogin[name]
	lanOnly := r.lanOnly[name]
	r.mu.RUnlock()
	// Two-rule cloud-side gate. Both rules apply only when the inbound
	// request carries X-Forwarded-Prefix (i.e. came via cloudbox).
	// Direct loopback hits (admin UI subpath, local /app/<name>/* via
	// the loopback main listener) are unaffected — the LAN/loopback
	// trust boundary is what gates them.
	if c.Request.Header.Get("X-Forwarded-Prefix") != "" {
		// Rule 1: require_login. Cloudbox stamps X-Periscope-Role
		// when the per-(host, app) matrix_elev cookie validates;
		// without that header, this request hasn't proven local-OS
		// auth and we refuse.
		if requireLogin && c.Request.Header.Get("X-Periscope-Role") == "" {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		// Rule 2: lan_only_paths. Kiosk-style endpoints must never
		// leak through the cloud surface. Segment-anchored prefix
		// match: "/kiosk" blocks "/kiosk" and "/kiosk/foo" but not
		// "/kiosks-of-truth".
		for _, p := range lanOnly {
			if matchSegmentPrefix(rest, p) {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
		}
	}
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
	// Let httputil.ReverseProxy's director do the target-path prefix
	// join — it uses the same single-slash semantics our `singleJoin`
	// did, but does it exactly once. Pre-joining here would prepend
	// target.Path twice for apps with a non-empty upstream base path.
	c.Request.URL.Path = rest
	c.Request.URL.RawPath = ""
	rp.ServeHTTP(c.Writer, c.Request)
}

// matchSegmentPrefix reports whether path begins with prefix at a
// path-segment boundary, so "/kiosk" matches "/kiosk" and
// "/kiosk/foo" but NOT "/kiosks-of-truth". Empty prefix never matches
// (it would block every cloud request — a foot-gun).
func matchSegmentPrefix(path, prefix string) bool {
	if prefix == "" {
		return false
	}
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, prefix+"/")
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
