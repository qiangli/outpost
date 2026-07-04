package tessaro

import (
	"context"
	"log/slog"
	"net/http"
)

type ctxKey int

const identityKey ctxKey = 0

// Guard holds the app's trust policy and produces the auth middleware. Construct
// one at startup from your config and reuse it for every protected route.
type Guard struct {
	// Secret is the per-app SSO secret shared with outpost (the "Trust cloudbox
	// identity" toggle). Empty disables HMAC — only safe when the upstream is
	// loopback-only AND RequireHMAC is false.
	Secret []byte
	// RequireHMAC rejects cloud-arrived requests that lack a valid signature.
	// Keep true whenever the upstream port is reachable beyond loopback.
	RequireHMAC bool
	// AdminEmails is the app-internal admin allowlist (RBAC), keyed off the
	// verified Remote-User. This is how admin-vs-regular is enforced at the app
	// boundary; cloudbox does not do it for you.
	AdminEmails map[string]bool
	Log         *slog.Logger
}

// verify establishes a trusted Identity for the request, or returns false.
//   - Cloud-arrived (X-Forwarded-Prefix present): require a valid HMAC signature
//     when RequireHMAC or a secret is set; otherwise trust the stamp.
//   - Direct/loopback: no cloud identity; treated as unauthenticated for web
//     RBAC (superadmin/local flows are handled separately by the app).
func (g *Guard) verify(r *http.Request) (Identity, bool) {
	if !ArrivedViaCloud(r) {
		return Identity{}, false
	}
	if g.RequireHMAC || len(g.Secret) > 0 {
		if !VerifyOutpost(r, g.Secret) {
			return Identity{}, false
		}
	}
	id := IdentityFrom(r)
	return id, id.Authenticated()
}

// RequireAuth admits any cloud-vouched (and, per policy, HMAC-verified) caller.
func (g *Guard) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := g.verify(r)
		if !ok {
			http.Error(w, "authentication required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, id)))
	})
}

// RequireAdmin admits only verified callers whose email is in AdminEmails. This
// is pattern (a) from the contract: accept the cloud stamp as identity, do your
// own RBAC. An empty AdminEmails admits any cloud-admin-tier caller (Groups=="admin")
// — convenient for a demo, but real apps should populate the allowlist.
func (g *Guard) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := g.verify(r)
		if !ok {
			http.Error(w, "authentication required", http.StatusForbidden)
			return
		}
		if !g.isAdmin(id) {
			http.Error(w, "admin privileges required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, id)))
	})
}

// RequireLocal admits only requests that did NOT arrive through the cloud tunnel
// — the app-side half of an `lan_only_paths` route. Outpost 404s these paths for
// web callers before they reach the app; this is defense in depth (and covers
// the app being reached directly on a non-loopback LAN address).
//
// This is where a real app's SUPERADMIN gate lives: on the LAN, additionally
// prove local presence with OS auth (e.g. a PAM-backed /admin/login), since
// superadmin must never be grantable by cloud identity. The scaffold stops at
// the LAN check and leaves the OS-auth step as a documented extension point.
func (g *Guard) RequireLocal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ArrivedViaCloud(r) {
			http.NotFound(w, r)
			return
		}
		// TODO(superadmin): gate with OS auth here before destructive actions.
		next.ServeHTTP(w, r)
	})
}

func (g *Guard) isAdmin(id Identity) bool {
	if len(g.AdminEmails) == 0 {
		return id.CloudAdmin()
	}
	return g.AdminEmails[id.User]
}

// IdentityOf returns the verified identity stored by RequireAuth/RequireAdmin.
func IdentityOf(r *http.Request) (Identity, bool) {
	id, ok := r.Context().Value(identityKey).(Identity)
	return id, ok
}
