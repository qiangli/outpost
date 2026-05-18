package agent

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// AppRegistry maps app names (e.g. "ycode") to the local URL they live at
// (e.g. "http://127.0.0.1:8765"). It is loopback-only by design: the agent
// itself is only reachable through the frp tunnel, and the agent only
// forwards under tier-1 trust (set by the cloud server in the
// X-Periscope-User header).
type AppRegistry struct {
	mu    sync.RWMutex
	apps  map[string]*url.URL
	proxy map[string]*httputil.ReverseProxy
}

func NewAppRegistry() *AppRegistry {
	return &AppRegistry{
		apps:  map[string]*url.URL{},
		proxy: map[string]*httputil.ReverseProxy{},
	}
}

// Register adds (or replaces) an app entry. target must be an absolute URL.
func (r *AppRegistry) Register(name, target string) error {
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("app %q target: %w", name, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("app %q target must be absolute (got %q)", name, target)
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	r.mu.Lock()
	r.apps[name] = u
	r.proxy[name] = rp
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

// LookupTarget returns the registered target URL (or nil).
func (r *AppRegistry) LookupTarget(name string) *url.URL {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.apps[name]
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
