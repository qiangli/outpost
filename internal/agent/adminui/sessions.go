package adminui

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sessionStore mints and validates HMAC-signed admin session cookies.
//
// Cookies are stateless: `user|expUnixSecs|sigBase64`. Validation
// re-HMACs the prefix with the server's secret and compares. This means
// the session SURVIVES process restarts (the secret is loaded from the
// FileConfig on every boot), so flipping a built-in toggle — which
// re-execs the binary — no longer logs the admin user out.
//
// Revocation is best-effort: an in-memory set tracks cookies that were
// explicitly logged out. The set is lost on restart, but the cookie that
// was revoked is also gone from the user's browser (we sent MaxAge=-1),
// so this only matters if someone copied the cookie elsewhere — and in
// that case the bounded 1h TTL caps the exposure.
type sessionStore struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	revoked map[string]time.Time
}

// newSessionStore builds a session store with the given TTL and signing
// key. An empty secret causes a one-shot random key to be generated —
// useful for tests; production paths should pass a persisted key so
// sessions outlive process restarts.
func newSessionStore(ttl time.Duration, secret []byte) *sessionStore {
	if len(secret) == 0 {
		var b [32]byte
		_, _ = rand.Read(b[:])
		secret = b[:]
	}
	return &sessionStore{
		secret:  secret,
		ttl:     ttl,
		now:     time.Now,
		revoked: map[string]time.Time{},
	}
}

// Mint returns a signed session cookie. The user string is opaque to the
// store — it's encoded into the cookie verbatim and surfaced from
// Validate. Must not contain '|' (the field separator). Practically the
// only caller is handleLogin, which passes hostauth.CurrentUser() — a
// plain OS user name.
func (s *sessionStore) Mint(user string) (string, error) {
	if strings.ContainsRune(user, '|') {
		// Defensive: refuse separator chars so the format stays unambiguous.
		return "", errSeparatorInUser
	}
	exp := s.now().Add(s.ttl).Unix()
	payload := user + "|" + strconv.FormatInt(exp, 10)
	sig := s.sign(payload)
	return payload + "|" + sig, nil
}

// Validate returns the user owning the cookie if signature checks pass
// and the cookie isn't expired or revoked.
func (s *sessionStore) Validate(cookie string) (string, bool) {
	if cookie == "" {
		return "", false
	}
	// Reject revoked-in-this-process cookies before any HMAC work.
	s.mu.Lock()
	_, gone := s.revoked[cookie]
	s.mu.Unlock()
	if gone {
		return "", false
	}
	parts := strings.SplitN(cookie, "|", 3)
	if len(parts) != 3 {
		return "", false
	}
	user, expStr, sig := parts[0], parts[1], parts[2]
	want := s.sign(user + "|" + expStr)
	// Constant-time compare to avoid timing leaks of the signature.
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return "", false
	}
	expUnix, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return "", false
	}
	if s.now().Unix() > expUnix {
		return "", false
	}
	return user, true
}

// Revoke best-effort invalidates a cookie for this process's lifetime.
// Stateless cookies can't be truly recalled; the revocation set + the
// browser-side cookie clear (Set-Cookie MaxAge=-1) together give logout
// the user-visible behavior they expect.
func (s *sessionStore) Revoke(cookie string) {
	if cookie == "" {
		return
	}
	s.mu.Lock()
	s.revoked[cookie] = s.now()
	s.mu.Unlock()
}

func (s *sessionStore) sign(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// errSeparatorInUser is the sentinel for the unreachable Mint() guard.
// Defined as a package var to avoid stringly-typed errors in callers.
var errSeparatorInUser = sessionMintError("user name may not contain '|'")

type sessionMintError string

func (e sessionMintError) Error() string { return string(e) }
