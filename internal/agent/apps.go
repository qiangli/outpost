package agent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

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
// itself is only reachable through the frp tunnel, and the agent only
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
}

func NewAppRegistry() *AppRegistry {
	return &AppRegistry{
		apps:  map[string]*url.URL{},
		proxy: map[string]*httputil.ReverseProxy{},
		roles: map[string]string{},
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
	r.mu.Unlock()
	return nil
}

// Names returns registered app names.
func (r *AppRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.apps))
	for k := range r.apps {
		out = append(out, k)
	}
	return out
}

// Entries returns the registered apps with their declared roles. Used by
// GET /apps so the cloud sees per-app role declarations and can gate
// custom apps accurately.
func (r *AppRegistry) Entries() []AppEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AppEntry, 0, len(r.apps))
	for name := range r.apps {
		role := r.roles[name]
		if role == "" {
			role = "user"
		}
		out = append(out, AppEntry{Name: name, Role: role})
	}
	return out
}

// LookupTarget returns the registered target URL (or nil).
func (r *AppRegistry) LookupTarget(name string) *url.URL {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.apps[name]
}

// Unregister removes an app entry. No-op if the name is not registered.
func (r *AppRegistry) Unregister(name string) {
	r.mu.Lock()
	delete(r.apps, name)
	delete(r.proxy, name)
	delete(r.roles, name)
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
	default:
		return fmt.Errorf("app %q: scheme must be one of http|https|unix|npipe (got %q)", ac.Name, ac.Scheme)
	}
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
		name := c.Param("name")
		r.mu.RLock()
		rp := r.proxy[name]
		target := r.apps[name]
		r.mu.RUnlock()
		if rp == nil {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "unknown app: " + name})
			return
		}

		// rest is the captured wildcard, e.g. "/api/sessions" — leading slash
		// is included when present, empty for the root.
		rest := c.Param("p")
		if rest == "" {
			rest = "/"
		}

		// Splice in a Director that rewrites the path while keeping the
		// per-app ReverseProxy's default behavior (host, scheme, etc.).
		origDirector := rp.Director
		c.Request.URL.Path = singleJoin(target.Path, rest)
		c.Request.URL.RawPath = ""
		_ = origDirector // already captured target in NewSingleHostReverseProxy
		rp.ServeHTTP(c.Writer, c.Request)
	}
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
