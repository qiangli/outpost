package agent

import (
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// A site app (an http app registered with MCPPort) answers MCP on its own
// origin: <app-mount>/mcp is the Streamable HTTP endpoint (POST plus the SSE
// GET on one path), reverse-proxied to the owner's `bashy mcp serve
// --transport http --tools site --site-app <name> --outpost` on loopback.
// It is a proxy, never a redirect: the caller keeps the page's cookie, the
// require_login gate is the pages' gate, and the request is stamped with
// the same signed identity and relationship headers as the pages, so the
// engine sees the principal the pages see and decides each tool by tier.
const siteMCPPath = "/mcp"

// siteMCPMetadataPath is the RFC 9728 protected-resource metadata, served at
// the mount root and in its resource-suffixed form.
const siteMCPMetadataPath = "/.well-known/oauth-protected-resource"

func isSiteMCPMetadataPath(rest string) bool {
	return rest == siteMCPMetadataPath || rest == siteMCPMetadataPath+siteMCPPath
}

// siteMCPProxy derives the MCP proxy from the app's page proxy: its Rewrite
// runs first (forwarded headers, identity strip and re-stamp), then only the
// upstream URL moves to the engine. FlushInterval -1 streams SSE unbuffered.
func siteMCPProxy(pages *httputil.ReverseProxy, port int) *httputil.ReverseProxy {
	if pages == nil || port <= 0 || port > 65535 {
		return nil
	}
	engine := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	return &httputil.ReverseProxy{
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "site mcp engine unavailable", http.StatusBadGateway)
		},
		Rewrite: func(pr *httputil.ProxyRequest) {
			pages.Rewrite(pr)
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", engine
			pr.Out.URL.Path, pr.Out.URL.RawPath = siteMCPPath, ""
			pr.Out.Host = ""
		},
	}
}

// siteOrigin is the site as the caller sees it: scheme://host plus the app
// mount the front forwarded (an empty forwarded prefix is a site at the root
// of its own host), falling back to the local /app/<name> mount.
func siteOrigin(r *http.Request, name string) string {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		proto = "http"
		if r.TLS != nil {
			proto = "https"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	prefix := "/app/" + name
	if values := r.Header.Values("X-Forwarded-Prefix"); len(values) > 0 {
		prefix = values[0]
	}
	return proto + "://" + host + strings.TrimRight(prefix, "/")
}

// serveSiteMCPMetadata points an MCP client that cannot carry the site cookie
// at the site's own sign-in: authorizing the client and logging in to the
// site are one step.
func serveSiteMCPMetadata(c *gin.Context, name string) {
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		c.AbortWithStatus(http.StatusMethodNotAllowed)
		return
	}
	origin := siteOrigin(c.Request, name)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"resource": origin + siteMCPPath, "authorization_servers": []string{origin}})
}

// challengeSiteMCP is the require_login refusal on the MCP path: a 401 whose
// WWW-Authenticate names the metadata, so a client starts the sign-in flow.
func challengeSiteMCP(c *gin.Context, name string) {
	c.Header("WWW-Authenticate", `Bearer resource_metadata="`+siteOrigin(c.Request, name)+siteMCPMetadataPath+`"`)
	c.AbortWithStatus(http.StatusUnauthorized)
}
