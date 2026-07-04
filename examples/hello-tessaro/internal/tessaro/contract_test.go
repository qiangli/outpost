package tessaro

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// sign builds a request stamped exactly as outpost would, for the given secret.
func sign(secret, user, groups, prefix string, ts int64) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/app", nil)
	r.Header.Set(HdrForwardedPrefix, prefix)
	r.Header.Set(HdrRemoteUser, user)
	r.Header.Set(HdrRemoteGroups, groups)
	r.Header.Set(HdrIdentityTs, strconv.FormatInt(ts, 10))
	payload := user + "\n" + groups + "\n" + prefix + "\n" + strconv.FormatInt(ts, 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	r.Header.Set(HdrIdentitySig, hex.EncodeToString(mac.Sum(nil)))
	return r
}

func TestVerifyOutpost(t *testing.T) {
	const secret = "s3cr3t"
	const prefix = "/matrix/h/dragon/app/hello-tessaro-dev"
	now := time.Now().Unix()

	cases := []struct {
		name string
		r    *http.Request
		want bool
	}{
		{"valid", sign(secret, "a@x.io", "admin", prefix, now), true},
		{"stale", sign(secret, "a@x.io", "admin", prefix, now-120), false},
		{"future", sign(secret, "a@x.io", "admin", prefix, now+120), false},
		{"wrong secret", sign("other", "a@x.io", "admin", prefix, now), false},
		{"tampered user", tamper(sign(secret, "a@x.io", "admin", prefix, now), HdrRemoteUser, "evil@x.io"), false},
		{"tampered role", tamper(sign(secret, "a@x.io", "user", prefix, now), HdrRemoteGroups, "admin"), false},
		{"no prefix (direct)", stripHdr(sign(secret, "a@x.io", "admin", prefix, now), HdrForwardedPrefix), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyOutpost(c.r, []byte(secret)); got != c.want {
				t.Fatalf("VerifyOutpost = %v, want %v", got, c.want)
			}
		})
	}
}

func TestVerifyOutpostEmptySecret(t *testing.T) {
	r := sign("", "a@x.io", "admin", "/p", time.Now().Unix())
	if VerifyOutpost(r, nil) {
		t.Fatal("empty secret must never verify")
	}
}

func TestGuardRBAC(t *testing.T) {
	const secret = "k"
	const prefix = "/matrix/h/h/app/a"
	g := &Guard{Secret: []byte(secret), RequireHMAC: true, AdminEmails: map[string]bool{"boss@x.io": true}}

	// admin allowlist member reaches an admin route
	adminReq := sign(secret, "boss@x.io", "admin", prefix, time.Now().Unix())
	if rec := serve(g.RequireAdmin(ok()), adminReq); rec.Code != http.StatusOK {
		t.Fatalf("allowlisted admin: got %d", rec.Code)
	}
	// a cloud-admin-tier caller NOT on the allowlist is refused (app RBAC wins)
	strangerReq := sign(secret, "rando@x.io", "admin", prefix, time.Now().Unix())
	if rec := serve(g.RequireAdmin(ok()), strangerReq); rec.Code != http.StatusForbidden {
		t.Fatalf("non-allowlisted admin: got %d, want 403", rec.Code)
	}
	// same stranger passes RequireAuth (they are a valid user)
	if rec := serve(g.RequireAuth(ok()), strangerReq); rec.Code != http.StatusOK {
		t.Fatalf("valid user on RequireAuth: got %d", rec.Code)
	}
	// unsigned cloud request is rejected under RequireHMAC
	unsigned := httptest.NewRequest(http.MethodGet, "/app", nil)
	unsigned.Header.Set(HdrForwardedPrefix, prefix)
	unsigned.Header.Set(HdrRemoteUser, "boss@x.io")
	if rec := serve(g.RequireAuth(ok()), unsigned); rec.Code != http.StatusForbidden {
		t.Fatalf("unsigned under RequireHMAC: got %d, want 403", rec.Code)
	}
}

func TestPrefixHelpers(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(HdrForwardedPrefix, "/matrix/h/dragon/app/a/")
	if got := BasePrefix(r); got != "/matrix/h/dragon/app/a" {
		t.Fatalf("BasePrefix trailing slash not trimmed: %q", got)
	}
	if got := BaseHref(r); got != "/matrix/h/dragon/app/a/" {
		t.Fatalf("BaseHref = %q", got)
	}
	if got := PrefixPath(r, "app"); got != "/matrix/h/dragon/app/a/app" {
		t.Fatalf("PrefixPath = %q", got)
	}
	// direct request: no prefix
	d := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := PrefixPath(d, "/x"); got != "/x" {
		t.Fatalf("direct PrefixPath = %q", got)
	}
}

func tamper(r *http.Request, h, v string) *http.Request { r.Header.Set(h, v); return r }
func stripHdr(r *http.Request, h string) *http.Request  { r.Header.Del(h); return r }
func ok() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}
func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}
