// In-memory session manager for the HTTP `/api/v1/discover/*` flow.
//
// Sessions are short-lived (5 min default) and bounded (256 entries).
// They carry the nonce we issued to the caller for `/probe` and a
// `verified` flag flipped after the signed-nonce check passes.
//
// We deliberately don't persist sessions; an outpost restart drops
// in-flight handshakes and callers re-handshake. That keeps the
// security surface small and the implementation trivial.
package discovery

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

const (
	defaultSessionTTL  = 5 * time.Minute
	defaultMaxSessions = 256
)

// Session is one in-flight or completed discovery handshake.
type Session struct {
	ID        string
	PeerID    PeerID
	Nonce     []byte // we issued this; caller signs it in /probe
	Verified  bool   // flipped true after a successful /probe
	ExpiresAt time.Time
}

// SessionStore is the in-memory cap-bounded session table. Safe for
// concurrent use.
type SessionStore struct {
	mu      sync.Mutex
	byID    map[string]*Session
	ttl     time.Duration
	maxSize int
}

// NewSessionStore returns an empty store with defaults.
func NewSessionStore() *SessionStore {
	return &SessionStore{
		byID:    make(map[string]*Session),
		ttl:     defaultSessionTTL,
		maxSize: defaultMaxSessions,
	}
}

// New mints a fresh session for the given peer with a freshly-generated
// 32-byte nonce. The caller should send back `{session_id, nonce}` in
// the /hello response.
//
// When the store is at capacity, the oldest session is evicted to make
// room — small-scale outposts are unlikely to ever hit this.
func (s *SessionStore) New(peer PeerID) (*Session, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	id := newSessionID()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked(time.Now())
	if len(s.byID) >= s.maxSize {
		s.evictOldestLocked()
	}
	sess := &Session{
		ID:        id,
		PeerID:    peer,
		Nonce:     nonce,
		ExpiresAt: time.Now().Add(s.ttl),
	}
	s.byID[id] = sess
	return sess, nil
}

// Get returns the session by ID, or (nil, false) when absent/expired.
// A get on an expired session evicts it as a side effect — saves a
// separate sweep goroutine.
func (s *SessionStore) Get(id string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	if time.Now().After(sess.ExpiresAt) {
		delete(s.byID, id)
		return nil, false
	}
	return sess, true
}

// MarkVerified flips a session's verified flag (after a successful
// /probe signature check). No-op when the session has already expired.
func (s *SessionStore) MarkVerified(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		delete(s.byID, id)
		return
	}
	sess.Verified = true
}

// Len reports the current number of live sessions. For tests.
func (s *SessionStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked(time.Now())
	return len(s.byID)
}

// evictExpiredLocked drops every session whose TTL has elapsed.
// Caller must hold s.mu.
func (s *SessionStore) evictExpiredLocked(now time.Time) {
	for id, sess := range s.byID {
		if now.After(sess.ExpiresAt) {
			delete(s.byID, id)
		}
	}
}

// evictOldestLocked drops the session with the earliest ExpiresAt.
// Caller must hold s.mu.
func (s *SessionStore) evictOldestLocked() {
	var oldestID string
	var oldestAt time.Time
	first := true
	for id, sess := range s.byID {
		if first || sess.ExpiresAt.Before(oldestAt) {
			oldestID = id
			oldestAt = sess.ExpiresAt
			first = false
		}
	}
	if oldestID != "" {
		delete(s.byID, oldestID)
	}
}

// newSessionID returns a 22-char base64 URL-safe random ID. 16 bytes
// of entropy is plenty for a session-collision check; we want
// human-readable IDs in logs without going to full UUIDs.
func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
