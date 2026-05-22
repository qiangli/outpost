package adminui

import (
	"sync"
	"time"
)

// loginLimiter is a per-IP token bucket for POST /api/login. PAM auth on
// the success path is already a slow operation, but a determined LAN
// brute-forcer can still chew through credentials without a throttle.
// This is intentionally small (in-memory, per-process) — sessions expire
// in an hour and the process restarts on pairing changes, so a long-
// horizon attacker has many natural resets to deal with anyway.
type loginLimiter struct {
	mu     sync.Mutex
	cap    int
	refill time.Duration
	now    func() time.Time
	tab    map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// newLoginLimiter builds a limiter that allows up to cap attempts in a
// burst, then refills one token every refill interval. A bucket starts
// full (cap tokens) so a legitimate first-login isn't held up.
func newLoginLimiter(cap int, refill time.Duration) *loginLimiter {
	return &loginLimiter{
		cap:    cap,
		refill: refill,
		now:    time.Now,
		tab:    map[string]*bucket{},
	}
}

// Allow consumes one token from ip's bucket and reports whether the call
// is permitted. Callers MUST use this only on the *unauthenticated* login
// path — once a session cookie is in play, callers are already
// rate-limited by the speed of password retries.
func (l *loginLimiter) Allow(ip string) bool {
	if ip == "" {
		ip = "_unknown_"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.tab[ip]
	if !ok {
		b = &bucket{tokens: float64(l.cap), last: now}
		l.tab[ip] = b
	}
	// Refill since the last hit.
	elapsed := now.Sub(b.last)
	if elapsed > 0 && l.refill > 0 {
		b.tokens += float64(elapsed) / float64(l.refill)
		if b.tokens > float64(l.cap) {
			b.tokens = float64(l.cap)
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
