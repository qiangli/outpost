package admincore

import (
	"fmt"
	"strings"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// reservedNames are the route prefixes the admin UI / MCP server own.
// An app or outbound mount with one of these names would shadow either
// the admin REST API, the SPA assets, the health probe, or the MCP
// server — all of which sit on the same loopback listener.
var reservedNames = map[string]struct{}{
	"api":        {},
	"static":     {},
	"healthz":    {},
	"index.html": {},
	"app":        {},
	"mcp":        {},
	"_periscope": {},
}

func isReserved(name string) bool {
	_, ok := reservedNames[strings.ToLower(name)]
	return ok
}

// ValidateApp normalizes ac in place (lowercasing scheme, defaulting
// host, trimming whitespace) and rejects invalid combinations. Returns
// *APIError so callers can map straight to a transport status code.
//
// Same rules the admin SPA enforces client-side, replicated here as the
// authoritative gate.
func ValidateApp(ac *conf.AppConfig) error {
	ac.Name = strings.TrimSpace(ac.Name)
	if ac.Name == "" {
		return badRequest("name is required")
	}
	if strings.ContainsAny(ac.Name, "/ \t") {
		return badRequest("name cannot contain slashes or whitespace")
	}
	if isReserved(ac.Name) {
		return badRequest("name %q is reserved by the admin UI", ac.Name)
	}
	ac.Scheme = strings.ToLower(strings.TrimSpace(ac.Scheme))
	if ac.Scheme == "" {
		ac.Scheme = "http"
	}
	switch ac.Scheme {
	case "http", "https", "tcp":
		ac.Host = strings.TrimSpace(ac.Host)
		if ac.Host == "" {
			ac.Host = "127.0.0.1"
		}
		if ac.Port < 1 || ac.Port > 65535 {
			return badRequest("port %d is out of range", ac.Port)
		}
		ac.Socket = ""
	case "unix", "npipe":
		ac.Socket = strings.TrimSpace(ac.Socket)
		if ac.Socket == "" {
			return badRequest("socket path is required for scheme %q", ac.Scheme)
		}
		ac.Host = ""
		ac.Port = 0
	default:
		return badRequest("scheme must be one of http|https|tcp|unix|npipe")
	}
	// Path-prefix fields: canonicalize so /admin, admin, and /admin/
	// all become "/admin". Empty input stays empty.
	if ac.IndexPath != "" {
		ac.IndexPath = normalizePathPrefix(ac.IndexPath)
	}
	cleaned := ac.LANOnlyPaths[:0]
	for _, p := range ac.LANOnlyPaths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.ContainsAny(p, " \t*?") {
			return badRequest("lan_only_paths entry %q must not contain whitespace or wildcards", p)
		}
		cleaned = append(cleaned, normalizePathPrefix(p))
	}
	ac.LANOnlyPaths = cleaned
	// Legacy Role field — LoadFile already migrated it into RequireLogin.
	ac.Role = ""
	return nil
}

// OutboundParams mirrors the wire payload of POST /api/outbound. Lifted
// out of adminui so MCP tools can populate the same struct without
// reaching across packages.
type OutboundParams struct {
	Path       string `json:"path"`
	Name       string `json:"name"`
	Host       string `json:"host"`
	User       string `json:"user"`
	Scheme     string `json:"scheme,omitempty"`
	LocalPort  int    `json:"local_port,omitempty"`
	TTLSeconds int64  `json:"ttl_seconds,omitempty"`
}

// ValidateOutbound trims, normalizes, and rejects bad combinations on
// p. After a successful call p.Scheme is one of "" (treated as "http"),
// "tcp", or "ssh"; required fields per-scheme are non-empty.
func ValidateOutbound(p *OutboundParams) error {
	p.Path = strings.TrimSpace(p.Path)
	p.Name = strings.TrimSpace(p.Name)
	p.Host = strings.TrimSpace(p.Host)
	p.User = strings.TrimSpace(p.User)
	if p.Path == "" || p.Host == "" || p.User == "" {
		return badRequest("path, host, and user are all required")
	}
	if p.TTLSeconds < 0 {
		return badRequest("ttl_seconds %d cannot be negative (use math.MaxInt64 for infinite)", p.TTLSeconds)
	}
	if strings.ContainsAny(p.Path, "/ \t") {
		return badRequest("path cannot contain slashes or whitespace")
	}
	if isReserved(p.Path) {
		return badRequest("path %q is reserved by the admin UI", p.Path)
	}
	p.Scheme = strings.ToLower(strings.TrimSpace(p.Scheme))
	switch p.Scheme {
	case "", "http":
		p.Scheme = "" // empty back-compat marker — defaults to "http"
		p.LocalPort = 0
		if p.Name == "" {
			return badRequest("name is required for http outbound")
		}
	case "tcp":
		if p.Name == "" {
			return badRequest("name is required for tcp outbound")
		}
		if p.LocalPort < 1 || p.LocalPort > 65535 {
			return badRequest("local_port %d is out of range (required for scheme tcp)", p.LocalPort)
		}
	case "ssh":
		// Targets the remote outpost's built-in /ssh endpoint — Name is
		// ignored, stored empty for clarity.
		p.Name = ""
		if p.LocalPort < 1 || p.LocalPort > 65535 {
			return badRequest("local_port %d is out of range (required for scheme ssh)", p.LocalPort)
		}
	default:
		return badRequest("scheme %q must be one of http|tcp|ssh", p.Scheme)
	}
	return nil
}

// normalizePathPrefix returns p with a leading slash and no trailing
// slash. Empty input stays empty.
func normalizePathPrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}

// CloudboxHTTPBase derives the HTTP(S) base URL of cloudbox from the
// matrix-tunnel pairing fields. Protocols are paired (wss↔https,
// websocket/ws/tcp↔http). Returns empty when the FileConfig isn't
// paired yet.
func CloudboxHTTPBase(fc *conf.FileConfig) string {
	if fc == nil || fc.ServerAddr == "" {
		return ""
	}
	scheme := "https"
	switch strings.ToLower(fc.Protocol) {
	case "wss":
		scheme = "https"
	case "ws", "websocket", "tcp", "":
		scheme = "http"
	}
	port := ""
	if fc.ServerPort != 0 && !((scheme == "https" && fc.ServerPort == 443) || (scheme == "http" && fc.ServerPort == 80)) {
		port = fmt.Sprintf(":%d", fc.ServerPort)
	}
	return scheme + "://" + fc.ServerAddr + port
}
