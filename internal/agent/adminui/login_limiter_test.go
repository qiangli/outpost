package adminui

import (
	"testing"
	"time"
)

func TestLoginLimiterBurstThenRefill(t *testing.T) {
	l := newLoginLimiter(3, time.Second)
	// Pin "now" so the refill math is deterministic. The first burst of
	// 3 should pass; the 4th must be refused.
	base := time.Now()
	l.now = func() time.Time { return base }
	for i := 0; i < 3; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("call %d denied during burst", i+1)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("4th call should be rate-limited")
	}
	// Different IP gets its own bucket.
	if !l.Allow("5.6.7.8") {
		t.Fatal("different IP must not share the bucket")
	}
	// After 1.1s of refill, 1.2.3.4 has a token again (one per second).
	l.now = func() time.Time { return base.Add(1100 * time.Millisecond) }
	if !l.Allow("1.2.3.4") {
		t.Fatal("expected a refilled token after 1.1s")
	}
}

func TestLoginLimiterEmptyIPCoalesces(t *testing.T) {
	l := newLoginLimiter(1, time.Hour)
	if !l.Allow("") {
		t.Fatal("empty IP first call should pass")
	}
	if l.Allow("") {
		t.Fatal("empty IP second call should be rate-limited")
	}
}
