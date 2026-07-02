package agent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/telemetry"
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
//   - Capabilities: optional typed-app advertisement. Currently used
//     for the built-in ollama proxy to surface {type:"llm"} so cloudbox
//     can fold it into the model pool without a separate probe. Nil
//     for everything else; omitted from JSON when nil (old cloudbox
//     ignores the field).
type AppEntry struct {
	Name         string `json:"name"`
	Scheme       string `json:"scheme,omitempty"`
	RequireLogin bool   `json:"require_login"`
	// ElevationRequired, when true, additionally demands the OS-password
	// (PAM) elevation at cloudbox — only meaningful when RequireLogin is
	// also true. Default false: a require_login app authenticates the
	// caller (owner or sharee) without forcing the owner through PAM.
	ElevationRequired bool             `json:"elevation_required,omitempty"`
	IndexPath         string           `json:"index_path,omitempty"`
	Capabilities      *AppCapabilities `json:"capabilities,omitempty"`
}

// AppCapabilities is a free-form typed-app descriptor. Type is the
// only required field — it tells cloudbox "treat this app as a thing
// of class X." Currently the only recognized value is "llm" (for the
// built-in ollama proxy); cloudbox feature-detects unknown types so
// adding more later is backwards-compatible.
//
// Pointer-shaped so a missing Capabilities serializes as null/omit
// rather than as an empty object.
type AppCapabilities struct {
	Type string `json:"type"`
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
	requireLogin       map[string]bool
	elevationRequired  map[string]bool
	lanOnly            map[string][]string
	indexPath          map[string]string
	capabilities       map[string]*AppCapabilities
	trustCloudIdentity map[string]bool
	// provisioningTokens maps app name → bearer token; the inverse map
	// (token → app name) is built lazily by LookupByProvisioningToken to
	// stay O(1) under r.mu without paying for the inverse on every
	// register. The token only matters for the /_periscope/apps/<name>
	// relay endpoint — it does not affect proxy behavior.
	provisioningTokens map[string]string
	// ssoSecrets maps app name → HMAC key used to sign the identity
	// headers outpost stamps on proxied requests (X-Outpost-Identity-Sig
	// + X-Outpost-Identity-Ts). Read per-request inside the reverse-proxy
	// Rewrite callback; empty means no signature is stamped and the
	// cooperating app falls back to its own login UI.
	ssoSecrets map[string]string
	// intercepts is a per-app list of path-prefix → handler bindings.
	// When a /app/<name>/<rest> request matches one of an app's
	// intercept prefixes, the intercept fires instead of forwarding to
	// the reverse proxy. Used to attach metadata endpoints (e.g.
	// /_pool/capacity) on the same mount the rest of the app proxy
	// uses, so they ride the existing RequireLogin gate.
	intercepts map[string][]appIntercept
	// proxyWrap is an optional per-app middleware applied to the
	// reverse proxy's ServeHTTP. Used for in-flight request counting.
	// Nil entries pass through unchanged.
	proxyWrap map[string]func(http.Handler) http.Handler
	// tcp holds the raw host:port destination for tcp-scheme apps. Its
	// key set is disjoint from `proxy` — TCP apps don't speak HTTP, so
	// they get a dedicated WS↔TCP bridge handler rather than the
	// ReverseProxy path.
	tcp map[string]string
}

// appIntercept binds a path-prefix to a handler that pre-empts the
// reverse proxy for requests under one app. Prefix matching is
// segment-anchored so "/_pool" hits "/_pool" and "/_pool/foo" but not
// "/_poolish".
type appIntercept struct {
	prefix  string
	handler http.Handler
}

// identityHeaders is the trusted-header SSO header set that outpost
// owns end-to-end. Outpost strips these from every outgoing proxy
// request and only re-stamps them when the per-app TrustCloudIdentity
// flag is on AND the inbound request came through the matrix tunnel.
// Listed canonically (net/http normalizes on read/write); the strip is
// idempotent.
var identityHeaders = []string{
	"X-Periscope-User",
	"X-Periscope-Role",
	"Remote-User",
	"Remote-Email",
	"Remote-Name",
	"Remote-Groups",
	"X-Webauth-User",
	"X-Webauth-Email",
	"X-Webauth-Fullname",
	"X-Outpost-Identity-Sig",
	"X-Outpost-Identity-Ts",
}

// remoteNameFromEmail derives a Remote-Name value from an email-shaped
// identity. The Authelia / oauth2-proxy contract treats Remote-Name as
// a human display name distinct from Remote-User; when cloudbox doesn't
// supply one separately, the email's local-part is the best fallback.
// Returns "" when the input isn't email-shaped (so we don't pollute the
// header with raw subject IDs).
func remoteNameFromEmail(s string) string {
	at := strings.IndexByte(s, '@')
	if at <= 0 {
		return ""
	}
	return s[:at]
}

func isLoopbackRemote(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func localLoopbackIdentity() (user, email, role string) {
	user = strings.TrimSpace(os.Getenv("USER"))
	if user == "" {
		user = strings.TrimSpace(os.Getenv("LOGNAME"))
	}
	if user == "" {
		user = "local"
	}
	email = user
	if !strings.Contains(email, "@") {
		email += "@localhost"
	}
	return user, email, "admin"
}

func NewAppRegistry() *AppRegistry {
	return &AppRegistry{
		apps:               map[string]*url.URL{},
		proxy:              map[string]*httputil.ReverseProxy{},
		requireLogin:       map[string]bool{},
		elevationRequired:  map[string]bool{},
		lanOnly:            map[string][]string{},
		indexPath:          map[string]string{},
		capabilities:       map[string]*AppCapabilities{},
		trustCloudIdentity: map[string]bool{},
		provisioningTokens: map[string]string{},
		ssoSecrets:         map[string]string{},
		intercepts:         map[string][]appIntercept{},
		proxyWrap:          map[string]func(http.Handler) http.Handler{},
		tcp:                map[string]string{},
	}
}

// SetProvisioningToken records or clears the bearer that the user-sync
// relay endpoint (/_periscope/apps/<name>/users) accepts as the caller's
// proof of identity. Empty token clears the entry. Safe to call on an
// unknown name (token is stored regardless; if the app is later
// registered with the same name, lookups will find it).
func (r *AppRegistry) SetProvisioningToken(name, token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if token == "" {
		delete(r.provisioningTokens, name)
		return
	}
	r.provisioningTokens[name] = token
}

// LookupByProvisioningToken returns the name of the app whose
// ProvisioningToken matches the given bearer, or ("", false) on miss.
// O(n) over registered apps, which is acceptable — provisioning is a
// low-volume operation (user grants change infrequently).
func (r *AppRegistry) LookupByProvisioningToken(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for name, t := range r.provisioningTokens {
		if t == token {
			return name, true
		}
	}
	return "", false
}

// ProvisioningToken returns the bearer associated with name, or "" if
// none is set. Used by the admin UI to surface the token to the
// operator.
func (r *AppRegistry) ProvisioningToken(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.provisioningTokens[name]
}

// SetSSOSecret records or clears the HMAC key used to sign identity
// headers stamped on requests proxied to this app. Empty clears the
// entry. Read per-request inside the proxy Rewrite callback; safe to
// call on an unknown name.
func (r *AppRegistry) SetSSOSecret(name, secret string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if secret == "" {
		delete(r.ssoSecrets, name)
		return
	}
	r.ssoSecrets[name] = secret
}

// SSOSecret returns the HMAC key associated with name, or "" if none
// is set. Used by `outpost apps secret <name>` and the admin UI to
// surface the value to the operator.
func (r *AppRegistry) SSOSecret(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ssoSecrets[name]
}

// AddIntercept binds prefix → h under app `name`. Subsequent requests
// to /app/<name>/<rest> whose rest matches prefix (segment-anchored)
// are handled by h instead of forwarded to the reverse proxy. Multiple
// intercepts may be registered on the same app; matching is in
// registration order, longest prefix wins for equal-tied entries.
//
// No-op when name is unknown — the caller might decorate before the
// app's main proxy is registered; the metadata sticks until somebody
// queries Entries() (which doesn't care about intercepts) or the user
// calls Unregister (which wipes everything).
func (r *AppRegistry) AddIntercept(name, prefix string, h http.Handler) {
	if prefix == "" || h == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.intercepts[name] = append(r.intercepts[name], appIntercept{prefix: prefix, handler: h})
}

// SetProxyWrap attaches a middleware applied to the reverse-proxy
// handler when /app/<name>/<rest> proxies a non-intercept request.
// Passing nil clears any existing wrapper. Useful for instrumentation
// that needs to wrap the proxy itself (e.g. in-flight counters) rather
// than serve a sub-path.
func (r *AppRegistry) SetProxyWrap(name string, wrap func(http.Handler) http.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if wrap == nil {
		delete(r.proxyWrap, name)
		return
	}
	r.proxyWrap[name] = wrap
}

// AppMeta carries the access-control + display fields the registry
// associates with each app. Internal to register/registerTCP. Tests
// and the simple Register helper pass a zero-value or a partial value.
type AppMeta struct {
	RequireLogin bool
	// ElevationRequired demands OS-password (PAM) elevation in addition to
	// authentication. Only consulted when RequireLogin is true.
	ElevationRequired bool
	LANOnlyPaths      []string
	IndexPath         string
	// Capabilities is the optional typed-app advertisement (e.g.
	// {Type:"llm"} for the built-in ollama proxy). Nil for vanilla
	// HTTP apps.
	Capabilities *AppCapabilities
	// TrustCloudIdentity opts the app into the trusted-header SSO
	// contract — outpost stamps Remote-User / Remote-Email /
	// Remote-Groups (and passes through X-Periscope-User /
	// X-Periscope-Role) on requests that came through the matrix
	// tunnel. Off by default; the per-app sanitize pass strips any
	// inbound copies of these headers regardless.
	TrustCloudIdentity bool
	// SSOSecret is the HMAC key outpost signs the identity headers
	// with so the upstream app can verify the stamp came from outpost
	// (defends the LAN spoof window where an attacker could set
	// Remote-User on a request that bypasses outpost). Empty means no
	// signature is stamped — upstream falls back to its own login.
	SSOSecret string
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
	trustIdentity := meta.TrustCloudIdentity
	rp := &httputil.ReverseProxy{
		Transport: transport,
		// Rewrite absolute-path Location headers (3xx redirects) so they
		// stay inside the public mount. Without this, an upstream that
		// returns `Location: /admin/login` (lots of CMS / classic web
		// apps do) makes the browser navigate to
		// https://<cloudbox>/admin/login — which on cloudbox is the
		// legacy `/admin/*p → /cloudbox/*p` redirect, dumping the user
		// into cloudbox itself instead of the app. Prepend our public
		// prefix (set by cloudbox via X-Forwarded-Prefix, e.g.
		// /h/host-a/app/lern-admin) so the redirect lands back on the
		// proxy.
		//
		// Untouched: full URLs (`https://other.example/foo`),
		// protocol-relative URLs (`//foo.example/x`), and paths already
		// prefixed by the mount (so a well-behaved app that already
		// honors X-Forwarded-Prefix doesn't get double-prefixed).
		ModifyResponse: func(resp *http.Response) error {
			// Echo the traceparent outpost saw back as a response
			// header so an end-to-end caller (ycode + curl) can
			// inspect the trace_id outpost actually received — load-
			// bearing for the collector-free e2e validation when no
			// OTLP collector is wired up at the outpost.
			if tp := resp.Request.Header.Get("traceparent"); tp != "" {
				resp.Header.Set("X-Outpost-Traceparent", tp)
			}
			loc := resp.Header.Get("Location")
			if loc == "" || loc[0] != '/' || (len(loc) >= 2 && loc[1] == '/') {
				return nil
			}
			prefix := resp.Request.Header.Get("X-Forwarded-Prefix")
			if prefix == "" || prefix == "/" {
				return nil
			}
			if loc == prefix || strings.HasPrefix(loc, prefix+"/") {
				return nil
			}
			resp.Header.Set("Location", prefix+loc)
			return nil
		},
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
			// Identity-header gate. Always strip the trusted-header SSO
			// fields from the outgoing request first — without this, a
			// LAN process could hit the loopback main listener with
			// `Remote-User: admin@…` and the upstream would honor it.
			// httputil.ReverseProxy clones pr.In's headers verbatim onto
			// pr.Out, so the inbound copy is still there until we
			// explicitly drop it.
			for _, h := range identityHeaders {
				pr.Out.Header.Del(h)
			}
			// Re-emit identity only when the app opted in via
			// TrustCloudIdentity and the identity source is vouched:
			// cloudbox supplies X-Periscope-* on matrix-origin
			// requests, while direct loopback requests use the local OS
			// user. Non-loopback direct requests never source identity.
			if trustIdentity {
				user := ""
				email := ""
				role := ""
				if pr.In.Header.Get("X-Forwarded-Prefix") != "" {
					user = pr.In.Header.Get("X-Periscope-User")
					email = user
					role = pr.In.Header.Get("X-Periscope-Role")
				} else if isLoopbackRemote(pr.In.RemoteAddr) {
					user, email, role = localLoopbackIdentity()
				}
				if user != "" {
					// Pass through the existing Periscope names so apps
					// written against the cooperative-web-apps doc keep
					// working unchanged.
					pr.Out.Header.Set("X-Periscope-User", user)
					// Mirror onto the Authelia / oauth2-proxy / nginx
					// trusted-header standard so off-the-shelf apps
					// (Grafana auth.proxy, Forgejo REVERSE_PROXY_*,
					// Sonarr/Radarr External, etc.) accept it without
					// custom code.
					pr.Out.Header.Set("Remote-User", user)
					pr.Out.Header.Set("X-WEBAUTH-USER", user)
					if email != "" {
						pr.Out.Header.Set("Remote-Email", email)
						pr.Out.Header.Set("X-WEBAUTH-EMAIL", email)
					}
					if name := remoteNameFromEmail(email); name != "" {
						pr.Out.Header.Set("Remote-Name", name)
						pr.Out.Header.Set("X-WEBAUTH-FULLNAME", name)
					}
				}
				if role != "" {
					pr.Out.Header.Set("X-Periscope-Role", role)
					pr.Out.Header.Set("Remote-Groups", role)
				}
				// HMAC-sign the identity tuple so the upstream app can
				// distinguish a real outpost stamp from a LAN attacker
				// who just sets Remote-User on a request that bypasses
				// outpost. Stamp only when a secret is configured —
				// empty secret means the cooperating app hasn't been
				// bootstrapped yet (operator runs `outpost apps secret
				// <name>` and pastes the value into the app's config),
				// in which case the app will refuse to honor identity
				// headers anyway. Read under RLock so a concurrent
				// rotate is safe.
				r.mu.RLock()
				secret := r.ssoSecrets[name]
				r.mu.RUnlock()
				if secret != "" {
					ts := strconv.FormatInt(time.Now().Unix(), 10)
					payload := user + "\n" + role + "\n" + pr.Out.Header.Get("X-Forwarded-Prefix") + "\n" + ts
					mac := hmac.New(sha256.New, []byte(secret))
					_, _ = mac.Write([]byte(payload))
					pr.Out.Header.Set("X-Outpost-Identity-Sig", hex.EncodeToString(mac.Sum(nil)))
					pr.Out.Header.Set("X-Outpost-Identity-Ts", ts)
				}
			}
			// Preserve the W3C trace context across the outpost-to-app
			// boundary. Cloudbox stamped a `traceparent` when it
			// forwarded this request through the matrix tunnel; the
			// gin tracing middleware extracted it into pr.In's context
			// (re-parented under outpost's own server span). Re-inject
			// onto pr.Out so the cooperative app can attach its spans
			// to the same trace. The contract works regardless of
			// whether OTEL_EXPORTER_OTLP_ENDPOINT is set on the
			// outpost — the global propagator is always installed.
			telemetry.PreserveTraceContext(pr.Out, pr.In)
		},
	}
	r.mu.Lock()
	r.apps[name] = target
	r.proxy[name] = rp
	r.requireLogin[name] = meta.RequireLogin
	r.elevationRequired[name] = meta.ElevationRequired
	r.lanOnly[name] = append([]string(nil), meta.LANOnlyPaths...)
	r.indexPath[name] = meta.IndexPath
	r.capabilities[name] = meta.Capabilities
	r.trustCloudIdentity[name] = meta.TrustCloudIdentity
	if s := strings.TrimSpace(meta.SSOSecret); s != "" {
		r.ssoSecrets[name] = s
	} else {
		delete(r.ssoSecrets, name)
	}
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
			Name:              name,
			Scheme:            scheme,
			RequireLogin:      r.requireLogin[name],
			ElevationRequired: r.elevationRequired[name],
			IndexPath:         r.indexPath[name],
			Capabilities:      r.capabilities[name],
		})
	}
	for name := range r.tcp {
		out = append(out, AppEntry{
			Name:              name,
			Scheme:            "tcp",
			RequireLogin:      r.requireLogin[name],
			ElevationRequired: r.elevationRequired[name],
			IndexPath:         r.indexPath[name],
			Capabilities:      r.capabilities[name],
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
	delete(r.elevationRequired, name)
	delete(r.lanOnly, name)
	delete(r.indexPath, name)
	delete(r.capabilities, name)
	delete(r.trustCloudIdentity, name)
	delete(r.provisioningTokens, name)
	delete(r.ssoSecrets, name)
	delete(r.intercepts, name)
	delete(r.proxyWrap, name)
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
		RequireLogin:       ac.RequireLogin,
		ElevationRequired:  ac.ElevationRequired,
		LANOnlyPaths:       ac.LANOnlyPaths,
		IndexPath:          ac.IndexPath,
		TrustCloudIdentity: ac.TrustCloudIdentity,
		SSOSecret:          ac.SSOSecret,
	}
	scheme := strings.ToLower(strings.TrimSpace(ac.Scheme))
	if scheme == "" {
		scheme = "http"
	}
	var err error
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
		err = r.register(ac.Name, u, meta, nil)
	case "unix", "npipe":
		sock := strings.TrimSpace(ac.Socket)
		if sock == "" {
			return fmt.Errorf("app %q: socket path is required for scheme %q", ac.Name, scheme)
		}
		// Synthetic http://socket; the real connection is dialed by the
		// per-app transport's DialContext. The upstream daemon (podman/
		// docker/ollama) ignores the Host header.
		u := &url.URL{Scheme: "http", Host: "socket"}
		err = r.register(ac.Name, u, meta, socketTransport(scheme, sock))
	case "tcp":
		host := strings.TrimSpace(ac.Host)
		if host == "" {
			host = "127.0.0.1"
		}
		if ac.Port <= 0 || ac.Port > 65535 {
			return fmt.Errorf("app %q: port %d is out of range", ac.Name, ac.Port)
		}
		err = r.registerTCP(ac.Name, net.JoinHostPort(host, strconv.Itoa(ac.Port)), meta)
	default:
		return fmt.Errorf("app %q: scheme must be one of http|https|tcp|unix|npipe (got %q)", ac.Name, ac.Scheme)
	}
	if err != nil {
		return err
	}
	// Provisioning token is independent of proxy plumbing — apps that
	// don't push grants leave it empty. Set unconditionally so a
	// re-register with a cleared token actually clears the entry.
	r.SetProvisioningToken(ac.Name, strings.TrimSpace(ac.ProvisioningToken))
	// SSO secret rides the same lifecycle — set unconditionally so a
	// re-register with a cleared secret clears the entry.
	r.SetSSOSecret(ac.Name, strings.TrimSpace(ac.SSOSecret))
	return nil
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
	r.elevationRequired[name] = meta.ElevationRequired
	r.lanOnly[name] = append([]string(nil), meta.LANOnlyPaths...)
	r.indexPath[name] = meta.IndexPath
	r.capabilities[name] = meta.Capabilities
	// TCP apps don't speak HTTP, so trust-cloud-identity has nothing to
	// stamp — but we keep the map entry in sync with the others so a
	// later HTTP-mode swap on the same name reads the right value.
	r.trustCloudIdentity[name] = meta.TrustCloudIdentity
	r.mu.Unlock()
	return nil
}

// SetCapabilities attaches (or clears, when caps is nil) a typed-app
// descriptor to an already-registered app. The capabilities-via-AppMeta
// path is for callers that construct the meta themselves; this helper
// is for the boot path in main.go, where built-ins register via
// RegisterFromConfig (which doesn't carry capability info) and then
// the caller decorates by name. No-op when name is unknown — we don't
// want a typo at boot to crash the agent.
func (r *AppRegistry) SetCapabilities(name string, caps *AppCapabilities) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, http := r.apps[name]; !http {
		if _, tcp := r.tcp[name]; !tcp {
			return
		}
	}
	r.capabilities[name] = caps
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
	intercepts := r.intercepts[name]
	wrap := r.proxyWrap[name]
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
	// Per-app intercept (e.g. /_pool/capacity on the ollama mount).
	// Runs after the cloud-side gate so intercepts inherit the same
	// auth posture as the proxy itself.
	if h := matchIntercept(intercepts, rest); h != nil {
		h.ServeHTTP(c.Writer, c.Request)
		return
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
	var h http.Handler = rp
	if wrap != nil {
		h = wrap(rp)
	}
	h.ServeHTTP(c.Writer, c.Request)
}

// matchIntercept returns the first intercept whose prefix is a
// segment-anchored prefix of rest. Iterates in registration order so
// callers can rely on first-registered-wins.
func matchIntercept(ints []appIntercept, rest string) http.Handler {
	for _, it := range ints {
		if matchSegmentPrefix(rest, it.prefix) {
			return it.handler
		}
	}
	return nil
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
