package agent

import (
	"context"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/jitter"
)

// The reconnect loop now uses decorrelated jitter (internal/jitter, with its
// own tests). Here we just pin the integration: fed the tunnel's own bounds,
// the backoff stays within [initial, max] across many iterations — so the
// reconnect can never sleep longer than the cap or shorter than the floor.
func TestReconnectBackoffBounds(t *testing.T) {
	prev, prevMax := reconnectInitialBackoff, reconnectMaxBackoff
	reconnectInitialBackoff = 100 * time.Millisecond
	reconnectMaxBackoff = 800 * time.Millisecond
	t.Cleanup(func() { reconnectInitialBackoff, reconnectMaxBackoff = prev, prevMax })

	b := reconnectInitialBackoff
	for i := 0; i < 1000; i++ {
		b = jitter.Backoff(b, reconnectInitialBackoff, reconnectMaxBackoff)
		if b < reconnectInitialBackoff || b > reconnectMaxBackoff {
			t.Fatalf("iter %d: backoff %v out of [%v,%v]", i, b, reconnectInitialBackoff, reconnectMaxBackoff)
		}
	}
}

// TestTunnelRunHonorsCtxCancellation builds a tunnel pointed at an
// unreachable server, lets the supervisor cycle through a couple of
// rebuilds, and verifies Run returns promptly when ctx is canceled
// (instead of hanging in a backoff sleep).
func TestTunnelRunHonorsCtxCancellation(t *testing.T) {
	prev, prevMax := reconnectInitialBackoff, reconnectMaxBackoff
	reconnectInitialBackoff = 50 * time.Millisecond
	reconnectMaxBackoff = 200 * time.Millisecond
	t.Cleanup(func() { reconnectInitialBackoff, reconnectMaxBackoff = prev, prevMax })

	// Port 1 is reserved/unused on every platform — the FRP client will
	// fail to dial and eventually return from Run, which is what we want
	// to drive the supervisor.
	tun, err := NewTunnel(TunnelConfig{
		ServerAddr: "127.0.0.1",
		ServerPort: 1,
		Token:      "test",
		User:       "test",
	}, nil, nil)
	if err != nil {
		t.Fatalf("NewTunnel: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tun.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned non-nil on ctx cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx deadline")
	}
}
