package adminui

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// sessionStore is a tiny in-memory cookie→user map. Process restart wipes
// it; that's fine — there is no admin UI to log into during a restart, and
// once the new process is up the user can log in again with their OS
// password.
type sessionStore struct {
	mu  sync.Mutex
	ttl time.Duration
	tab map[string]sessionEntry
	now func() time.Time
}

type sessionEntry struct {
	user      string
	expiresAt time.Time
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{
		ttl: ttl,
		tab: make(map[string]sessionEntry),
		now: time.Now,
	}
}

// Mint creates a new session for user and returns its opaque cookie value.
func (s *sessionStore) Mint(user string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	cookie := base64.RawURLEncoding.EncodeToString(b[:])
	s.mu.Lock()
	s.tab[cookie] = sessionEntry{user: user, expiresAt: s.now().Add(s.ttl)}
	s.mu.Unlock()
	return cookie, nil
}

// Validate returns the user owning the cookie if it is present and not
// expired. Expired entries are evicted on access.
func (s *sessionStore) Validate(cookie string) (string, bool) {
	if cookie == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.tab[cookie]
	if !ok {
		return "", false
	}
	if s.now().After(entry.expiresAt) {
		delete(s.tab, cookie)
		return "", false
	}
	return entry.user, true
}

// Revoke deletes a session if present.
func (s *sessionStore) Revoke(cookie string) {
	if cookie == "" {
		return
	}
	s.mu.Lock()
	delete(s.tab, cookie)
	s.mu.Unlock()
}
