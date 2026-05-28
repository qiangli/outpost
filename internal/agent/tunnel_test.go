package agent

import (
	"context"
	"testing"
	"time"
)

func TestGrowBackoff(t *testing.T) {
	prev, prevMax := reconnectInitialBackoff, reconnectMaxBackoff
	reconnectInitialBackoff = 100 * time.Millisecond
	reconnectMaxBackoff = 800 * time.Millisecond
	t.Cleanup(func() { reconnectInitialBackoff, reconnectMaxBackoff = prev, prevMax })

	got := []time.Duration{reconnectInitialBackoff}
	for i := 0; i < 6; i++ {
		got = append(got, growBackoff(got[len(got)-1]))
	}
	want := []time.Duration{
		100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond,
		800 * time.Millisecond, 800 * time.Millisecond, 800 * time.Millisecond,
		800 * time.Millisecond,
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("step %d: got %v want %v (sequence %v)", i, got[i], w, got)
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
