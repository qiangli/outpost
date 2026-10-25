package admincore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// reservedNames are the route prefixes the admin UI / MCP server own
// PLUS the cloudbox-side top-level namespace owners. An app or
// outbound mount with one of these names would either:
//
//   - Shadow a route on outpost's own loopback listener (admin REST,
//     SPA assets, health probe, MCP server, per-app provisioning
//     relay). That breaks the outpost host immediately.
//
//   - Shadow a cloudbox-side top-level namespace. That doesn't break
//     anything today — outpost-side apps proxy under /matrix/h/<host>
//     /app/<name>/, so the name is sandboxed at cloudbox-edge. But it
//     blocks future moves like vanity domain handling (`/<name>/`
//     mapped to an app via APP_DOMAINS) and pollutes the operator's
//     mental model. Cheap to refuse now while the field is small.
//
// The cloudbox-side blacklist mirrors hub/internal/reserved/Names. Keep
// the two in lockstep — if you add a top-level cloudbox prefix there,
// add the segment here too (and vice-versa).
var reservedNames = map[string]struct{}{
	// Outpost-loopback reservations.
	"api":        {},
	"static":     {},
	"healthz":    {},
	"index.html": {},
	"app":        {},
	"mcp":        {},
	"_periscope": {},
	// Cloudbox top-level namespace mirror.
	"about":       {},
	"cloudbox":    {},
	"contact":     {},
	"periscope":   {},
	"matrix":      {},
	"cloud":       {},
	"install":     {},
	"install.sh":  {},
	"install.ps1": {},
	"v1":          {},
	"v2":          {}, // cloudbox registry origin (/v2/*)
	"health":      {},
	"version":     {},
	"metrics":     {},
	"config":      {},
	"overlay":     {},
	"embed.js":    {},
	"favicon.ico": {},
	".well-known": {},
	"u":           {}, // public post viewer namespace (/u/<uid>/<path>)
	"superadmin":  {}, // master operator login (/superadmin/login, S498.1)
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
	case "http", "https", "tcp", "git":
		ac.Host = strings.TrimSpace(ac.Host)
		if ac.Host == "" {
			ac.Host = "127.0.0.1"
		}
		if ac.Scheme == "git" && ac.Port == 0 {
			// git+https upstreams live on 443 unless told otherwise.
			ac.Port = 443
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
	case "egress":
		// No fixed target — the allowlist is the declaration.
		// Blank entries are dropped; the rest must parse.
		ac.Host = ""
		ac.Port = 0
		ac.Socket = ""
		kept := ac.Allow[:0]
		for _, e := range ac.Allow {
			if strings.TrimSpace(e) == "" {
				continue
			}
			if _, _, _, ok := conf.SplitEgressAllowEntry(e); !ok {
				return badRequest("allow entry %q must be host, *.domain, with optional :port", e)
			}
			kept = append(kept, strings.TrimSpace(e))
		}
		ac.Allow = kept
	default:
		return badRequest("scheme must be one of http|https|tcp|unix|npipe|egress|git")
	}
	// Expose-token guard (S487.2): both fields or neither. Validate
	// the shape here — this is the authoritative gate every surface
	// (SPA / MCP / CLI) converges on. Messages never echo the digest.
	ac.ExposeTokenSHA256 = strings.ToLower(strings.TrimSpace(ac.ExposeTokenSHA256))
	ac.ExposeExpires = strings.TrimSpace(ac.ExposeExpires)
	switch {
	case ac.ExposeTokenSHA256 == "" && ac.ExposeExpires == "":
		// Unguarded — the historic behaviour.
	case ac.ExposeTokenSHA256 == "" || ac.ExposeExpires == "":
		return badRequest("expose_token_sha256 and expose_expires must be set together")
	default:
		if len(ac.ExposeTokenSHA256) != 2*sha256.Size {
			return badRequest("expose_token_sha256 must be 64 hex characters (got %d)", len(ac.ExposeTokenSHA256))
		}
		if _, err := hex.DecodeString(ac.ExposeTokenSHA256); err != nil {
			return badRequest("expose_token_sha256 must be 64 hex characters")
		}
		if _, err := time.Parse(time.RFC3339, ac.ExposeExpires); err != nil {
			return badRequest("expose_expires must be an RFC3339 timestamp")
		}
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
	// Git allowlist: shape-check + canonicalize whenever present. The
	// Allow field is shared with the egress kind, which validates its
	// own entries inside the switch above.
	if ac.Scheme == "git" && len(ac.Allow) > 0 {
		normalized, err := conf.NormalizeGitAllow(ac.Allow)
		if err != nil {
			return badRequest("%s", err.Error())
		}
		ac.Allow = normalized
	}
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
