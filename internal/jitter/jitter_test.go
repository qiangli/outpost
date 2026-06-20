package jitter

import (
	"testing"
	"time"
)

func TestBackoffBounds(t *testing.T) {
	base := 2 * time.Second
	max := 30 * time.Second
	// First attempt: prev == base → result in [base, 3*base].
	prev := base
	for i := 0; i < 1000; i++ {
		got := Backoff(prev, base, max)
		if got < base {
			t.Fatalf("backoff %v below base %v", got, base)
		}
		if got > max {
			t.Fatalf("backoff %v above max %v", got, max)
		}
		hi := prev * 3
		if hi > max {
			hi = max
		}
		if got > hi {
			t.Fatalf("backoff %v above decorrelated ceiling %v (prev=%v)", got, hi, prev)
		}
		prev = got
	}
}

func TestBackoffConvergesToCapWindow(t *testing.T) {
	// After enough failures the window saturates at max; results must
	// still never exceed max and should be able to reach near it.
	base, max := time.Second, 10*time.Second
	prev := max // already saturated
	sawHigh := false
	for i := 0; i < 1000; i++ {
		got := Backoff(prev, base, max)
		if got > max {
			t.Fatalf("backoff %v above max %v", got, max)
		}
		if got > max/2 {
			sawHigh = true
		}
		prev = got
	}
	if !sawHigh {
		t.Fatal("expected at least one backoff above max/2 when saturated")
	}
}

func TestBackoffDegenerate(t *testing.T) {
	// base==max → always exactly max, no panic.
	for i := 0; i < 100; i++ {
		if got := Backoff(5*time.Second, 5*time.Second, 5*time.Second); got != 5*time.Second {
			t.Fatalf("degenerate backoff = %v, want 5s", got)
		}
	}
	// zero/negative inputs must not panic.
	_ = Backoff(0, 0, 0)
	_ = Backoff(-1, -1, -1)
}

func TestFullBounds(t *testing.T) {
	w := 10 * time.Minute
	for i := 0; i < 1000; i++ {
		got := Full(w)
		if got < 0 || got >= w {
			t.Fatalf("Full(%v) = %v, out of [0,%v)", w, got, w)
		}
	}
	if Full(0) != 0 || Full(-time.Second) != 0 {
		t.Fatal("Full(<=0) must be 0")
	}
}

func TestFullSpreads(t *testing.T) {
	// Sanity: results should not all collapse to one value.
	w := time.Hour
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[Full(w)] = true
	}
	if len(seen) < 10 {
		t.Fatalf("Full not spreading: only %d distinct of 50", len(seen))
	}
}
