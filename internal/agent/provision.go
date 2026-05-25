package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// ProvisionDeps wires the relay endpoint that lets a cooperating app
// push its user grants up to cloudbox via outpost. The pieces are
// supplied at boot from main.go:
//
//   - Apps holds the live registry whose per-app ProvisioningToken
//     authenticates the caller.
//   - HTTPClient is the http.Client used to talk to cloudbox. Nil means
//     "use a default with a 30 s timeout". Tests inject a client with a
//     RoundTripper that points at a httptest.NewServer.
//   - CloudboxBase is the cloudbox HTTP(S) base URL (e.g.
//     https://ai.dhnt.io). Empty means outpost is unpaired and the
//     handler refuses with 503 — no cloudbox to forward to.
//   - AccessToken is outpost's bearer credential to cloudbox. Empty
//     means unpaired, same 503 path as CloudboxBase.
//   - AgentName is the host identity cloudbox knows this outpost by,
//     used in the cloudbox URL path (/api/hosts/<host>/apps/<name>/grants).
type ProvisionDeps struct {
	Apps         *AppRegistry
	HTTPClient   *http.Client
	CloudboxBase string
	AccessToken  string
	AgentName    string
}

// RegisterProvisionRoutes mounts the /_periscope/apps/:name/users
// surface on rg. Routes:
//
//	POST   /_periscope/apps/:name/users           upsert a grant
//	GET    /_periscope/apps/:name/users           list grants (30s cache)
//	DELETE /_periscope/apps/:name/users/:email    revoke a grant
//
// Mounted on the main loopback listener — never advertised through the
// matrix tunnel. Authentication is per-app bearer (AppConfig.
// ProvisioningToken); outpost forwards to cloudbox's grant API using its
// own access_token. Outpost does not persist grant state — cloudbox is
// the source of truth, GET is opportunistically cached for 30 s.
func RegisterProvisionRoutes(rg gin.IRouter, deps ProvisionDeps) {
	if deps.HTTPClient == nil {
		deps.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	cache := provisionCacheFor(deps.Apps)

	rg.POST("/_periscope/apps/:name/users", func(c *gin.Context) {
		app, ok := authenticateProvisioning(c, deps)
		if !ok {
			return
		}
		var g provisionGrant
		if err := c.ShouldBindJSON(&g); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		g.Email = strings.TrimSpace(strings.ToLower(g.Email))
		if g.Email == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "email is required"})
			return
		}
		if g.Role == "" {
			g.Role = "user"
		}
		payload, _ := json.Marshal(g)
		resp, err := doCloudbox(c, deps.HTTPClient, http.MethodPost,
			cloudboxGrantsBase(deps, app), deps.AccessToken, bytes.NewReader(payload))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		cache.invalidate(app)
		relayResponse(c, resp)
	})

	rg.GET("/_periscope/apps/:name/users", func(c *gin.Context) {
		app, ok := authenticateProvisioning(c, deps)
		if !ok {
			return
		}
		if body, fresh := cache.get(app); fresh {
			c.Data(http.StatusOK, "application/json", body)
			return
		}
		resp, err := doCloudbox(c, deps.HTTPClient, http.MethodGet,
			cloudboxGrantsBase(deps, app), deps.AccessToken, nil)
		if err != nil {
			// Stale-while-error fallback: a transient cloudbox hiccup
			// should not break the app's reconcile loop if we have any
			// historical view at all.
			if body, ok := cache.peek(app); ok {
				c.Data(http.StatusOK, "application/json", body)
				return
			}
			c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			cache.set(app, body)
			c.Data(resp.StatusCode, "application/json", body)
			return
		}
		if body2, ok := cache.peek(app); ok && resp.StatusCode >= 500 {
			c.Data(http.StatusOK, "application/json", body2)
			return
		}
		c.Data(resp.StatusCode, "application/json", body)
	})

	rg.DELETE("/_periscope/apps/:name/users/:email", func(c *gin.Context) {
		app, ok := authenticateProvisioning(c, deps)
		if !ok {
			return
		}
		email := strings.TrimSpace(strings.ToLower(c.Param("email")))
		if email == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "email is required"})
			return
		}
		target := cloudboxGrantsBase(deps, app) + "/" + url.PathEscape(email)
		resp, err := doCloudbox(c, deps.HTTPClient, http.MethodDelete,
			target, deps.AccessToken, nil)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		cache.invalidate(app)
		relayResponse(c, resp)
	})
}

// authenticateProvisioning resolves the bearer token to an app name and
// verifies the URL :name (if present) matches. Also checks cloudbox
// readiness. Returns the resolved app name and ok=true when the request
// should proceed; ok=false means a response has already been written
// and the caller should return.
func authenticateProvisioning(c *gin.Context, deps ProvisionDeps) (string, bool) {
	bearer := extractBearer(c.Request.Header.Get("Authorization"))
	if bearer == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "bearer token required"})
		return "", false
	}
	app, ok := deps.Apps.LookupByProvisioningToken(bearer)
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
		return "", false
	}
	pathName := strings.TrimSpace(c.Param("name"))
	if pathName != "" && pathName != app {
		c.AbortWithStatusJSON(http.StatusBadRequest,
			gin.H{"error": fmt.Sprintf("token belongs to app %q, not %q", app, pathName)})
		return "", false
	}
	if strings.TrimSpace(deps.CloudboxBase) == "" || strings.TrimSpace(deps.AccessToken) == "" ||
		strings.TrimSpace(deps.AgentName) == "" {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable,
			gin.H{"error": "outpost is not paired with cloudbox; grants cannot be forwarded"})
		return "", false
	}
	return app, true
}

func cloudboxGrantsBase(deps ProvisionDeps, app string) string {
	return strings.TrimRight(deps.CloudboxBase, "/") +
		"/api/hosts/" + url.PathEscape(deps.AgentName) +
		"/apps/" + url.PathEscape(app) + "/grants"
}

// extractBearer pulls the token out of "Authorization: Bearer <tok>".
// Empty when the header is missing or doesn't use the bearer scheme.
func extractBearer(h string) string {
	const prefix = "Bearer "
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, prefix))
}

// provisionGrant is the wire shape the relay accepts on POST. Mirrors
// what cloudbox stores in its sharee table. Role defaults to "user"
// when omitted; cloudbox treats unknown emails as pending grants (the
// tile appears on the user's dashboard once they register/log in).
type provisionGrant struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	Role  string `json:"role,omitempty"`
}

// doCloudbox executes one HTTP call against cloudbox with the bearer
// access_token. Body should be small (user grants are tiny payloads).
func doCloudbox(c *gin.Context, client *http.Client, method, target, token string, body io.Reader) (*http.Response, error) {
	if client == nil {
		return nil, errors.New("nil http client")
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), method, target, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return client.Do(req)
}

// relayResponse copies cloudbox's status + body to the caller. The body
// is JSON-ish; we don't try to wrap or transform it so cloudbox-side
// error messages reach the app developer cleanly.
func relayResponse(c *gin.Context, resp *http.Response) {
	body, _ := io.ReadAll(resp.Body)
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	c.Data(resp.StatusCode, ct, body)
}

// grantCache is a tiny TTL cache of cloudbox grant-list responses keyed
// by app name. Memory-only; lost on restart (cloudbox remains the
// source of truth).
type grantCache struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]grantCacheEntry
}

type grantCacheEntry struct {
	body      []byte
	expiresAt time.Time
}

func newGrantCache(ttl time.Duration) *grantCache {
	return &grantCache{ttl: ttl, entries: map[string]grantCacheEntry{}}
}

// get returns the cached body iff it's still fresh.
func (g *grantCache) get(name string) ([]byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[name]
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.body, true
}

// peek returns the cached body regardless of expiry — for
// stale-while-error fallback. Returns false only when nothing was ever
// cached for this app.
func (g *grantCache) peek(name string) ([]byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[name]
	if !ok {
		return nil, false
	}
	return e.body, true
}

func (g *grantCache) set(name string, body []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.entries[name] = grantCacheEntry{
		body:      append([]byte(nil), body...),
		expiresAt: time.Now().Add(g.ttl),
	}
}

func (g *grantCache) invalidate(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.entries, name)
}

// provisionCacheFor returns a process-wide cache instance keyed by
// AppRegistry pointer. POST/DELETE/GET share the same cache so an
// upsert immediately invalidates a later GET. Two AppRegistry instances
// (e.g. across tests) get independent caches.
var (
	provisionCacheMu sync.Mutex
	provisionCaches  = map[*AppRegistry]*grantCache{}
)

func provisionCacheFor(reg *AppRegistry) *grantCache {
	provisionCacheMu.Lock()
	defer provisionCacheMu.Unlock()
	if c, ok := provisionCaches[reg]; ok {
		return c
	}
	c := newGrantCache(30 * time.Second)
	provisionCaches[reg] = c
	return c
}
