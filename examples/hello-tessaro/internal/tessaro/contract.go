// Package tessaro implements the cloudbox/outpost cooperative-web-app contract:
// URL-prefix helpers, the SSO-HMAC identity check, and role middleware. This is
// the reusable half of the scaffold — copy this package into your own app and
// wire main.go to it. Nothing here depends on outpost source; it speaks only the
// public wire contract documented in outpost/docs/cooperative-web-apps.md.
package tessaro

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Wire header names stamped by outpost. These are part of the wire contract and
// must never be renamed (outposts in the wild depend on them).
const (
	HdrForwardedPrefix = "X-Forwarded-Prefix" // e.g. /matrix/h/<host>/app/<name>
	HdrForwardedHost   = "X-Forwarded-Host"
	HdrForwardedProto  = "X-Forwarded-Proto"
	HdrRemoteUser      = "Remote-User"   // cloud-vouched OAuth email
	HdrRemoteEmail     = "Remote-Email"  // same identity, explicit email header
	HdrRemoteName      = "Remote-Name"   // display name
	HdrRemoteGroups    = "Remote-Groups" // "admin" | "user" — cloud tier, NOT app RBAC
	HdrIdentityTs      = "X-Outpost-Identity-Ts"
	HdrIdentitySig     = "X-Outpost-Identity-Sig"
)

// clockSkew is the accepted signature age, matching the window outpost enforces.
const clockSkew = 60 * time.Second

// Identity is the cloud-vouched caller, as stamped by outpost. Groups=="admin"
// means the caller cleared cloudbox OAuth + the elevation gate at admin tier —
// it does NOT mean app-internal admin. Map it onto your own RBAC.
type Identity struct {
	User   string // email (Remote-User / Remote-Email)
	Name   string
	Groups string // "admin" | "user" | ""
}

// Authenticated reports whether outpost vouched for a caller at all.
func (id Identity) Authenticated() bool { return id.User != "" }

// CloudAdmin reports the cloud tier stamp only. Do not use this alone to gate
// destructive actions — combine with app RBAC (see RequireAdmin).
func (id Identity) CloudAdmin() bool { return id.Groups == "admin" }

// IdentityFrom extracts the stamped identity. It performs NO verification — call
// VerifyOutpost first (or use the Gate middleware) before trusting the values
// when the upstream port is reachable beyond loopback.
func IdentityFrom(r *http.Request) Identity {
	user := r.Header.Get(HdrRemoteUser)
	if user == "" {
		user = r.Header.Get(HdrRemoteEmail)
	}
	return Identity{
		User:   user,
		Name:   r.Header.Get(HdrRemoteName),
		Groups: r.Header.Get(HdrRemoteGroups),
	}
}

// ArrivedViaCloud reports whether the request came through the cloudbox→outpost
// tunnel (X-Forwarded-Prefix present) vs. directly on the LAN/loopback. Outpost
// strips this header on direct requests and only stamps it for tunnel traffic,
// so it is the single reliable "came from the web" signal.
func ArrivedViaCloud(r *http.Request) bool {
	return r.Header.Get(HdrForwardedPrefix) != ""
}

// VerifyOutpost returns true only when the request carries a valid, fresh
// SSO-HMAC signature over the canonical identity payload. When secret is empty,
// verification is impossible; callers MUST treat an empty secret as "cannot
// trust Remote-* beyond loopback" (see Gate).
//
// Canonical payload (newline-joined, no trailing newline):
//
//	<Remote-User>\n<Remote-Groups>\n<X-Forwarded-Prefix>\n<X-Outpost-Identity-Ts>
func VerifyOutpost(r *http.Request, secret []byte) bool {
	if len(secret) == 0 {
		return false
	}
	prefix := r.Header.Get(HdrForwardedPrefix)
	ts := r.Header.Get(HdrIdentityTs)
	sigHex := r.Header.Get(HdrIdentitySig)
	if prefix == "" || ts == "" || sigHex == "" {
		return false
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if d := time.Now().Unix() - t; d > int64(clockSkew.Seconds()) || d < -int64(clockSkew.Seconds()) {
		return false
	}
	user := r.Header.Get(HdrRemoteUser)
	if user == "" {
		user = r.Header.Get(HdrRemoteEmail)
	}
	role := r.Header.Get(HdrRemoteGroups)
	payload := user + "\n" + role + "\n" + prefix + "\n" + ts
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	want := mac.Sum(nil)
	got, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

// BasePrefix returns the URL prefix this app is mounted under (no trailing
// slash), or "" when the app is reached directly. Emit `<base href="{prefix}/">`
// (trailing slash is load-bearing) and build all links/assets relative.
func BasePrefix(r *http.Request) string {
	return strings.TrimRight(r.Header.Get(HdrForwardedPrefix), "/")
}

// PrefixPath joins the mount prefix with an absolute-rooted app path. Use it for
// any absolute path you must emit in a place outpost does NOT rewrite — JSON
// response bodies, redirects you build by hand, WebSocket URLs. Outpost rewrites
// Location headers for you, but never parses JSON bodies.
func PrefixPath(r *http.Request, rel string) string {
	if !strings.HasPrefix(rel, "/") {
		rel = "/" + rel
	}
	return BasePrefix(r) + rel
}

// BaseHref returns the value for the HTML <base href>, always trailing-slashed.
func BaseHref(r *http.Request) string {
	return BasePrefix(r) + "/"
}

// ExternalBase returns scheme://host as the browser sees it, derived from the
// forwarded headers (never Host/r.TLS, which reflect the loopback hop). Falls
// back to the direct request when not proxied. Use it to build absolute URLs and
// to validate Origin.
func ExternalBase(r *http.Request) string {
	proto := r.Header.Get(HdrForwardedProto)
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := r.Header.Get(HdrForwardedHost)
	if host == "" {
		host = r.Host
	}
	return proto + "://" + host
}
