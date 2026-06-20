// Package jitter provides randomized timing for retry/backoff loops so a
// fleet of hosts that fail or restart together does not retry in lockstep —
// a synchronized "thundering herd" that can overwhelm the shared cloudbox
// hub. See AWS Architecture Blog, "Exponential Backoff And Jitter".
//
// It draws from the default math/rand/v2 source, which is concurrency-safe
// and auto-seeded — no seeding required, and it deliberately does NOT touch
// the global math/rand state (keeping test determinism elsewhere intact).
package jitter

import (
	"math/rand/v2"
	"time"
)

// Backoff returns the next "decorrelated jitter" backoff: a uniform random
// duration in [base, prev*3], clamped to max. Feed the result back in as
// prev on the next attempt — the window grows like exponential backoff while
// the randomization breaks phase-lock between hosts. Pass prev=base for the
// first attempt.
//
// Compared to plain doubling, this keeps the self-correcting growth (a host
// that keeps failing backs off further) but guarantees two hosts that failed
// at the same instant pick different delays, so they stop retrying together.
func Backoff(prev, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = time.Millisecond
	}
	if max < base {
		max = base
	}
	hi := prev * 3
	if hi < base {
		hi = base
	}
	if hi > max {
		hi = max
	}
	// Uniform in [base, hi]. span+1 is always >= 1, so rand.N never panics.
	span := int64(hi - base)
	next := base
	if span > 0 {
		next += time.Duration(rand.N(span + 1))
	}
	return next
}

// Full returns a uniform random duration in [0, window) ("full jitter"). Use
// it to smear a fixed-interval timer (e.g. a poll loop, or a first-poll
// delay) across its whole window, so a fleet of hosts that started together
// does not fire together. Returns 0 for window <= 0.
func Full(window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	return time.Duration(rand.N(int64(window)))
}
