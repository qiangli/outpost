package supervisor

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// TestManager_RestartsOnExit: a fast-exiting program is restarted, so its
// start count climbs past 1 within a short window.
func TestManager_RestartsOnExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test child uses /bin/sh")
	}
	p := &Program{
		Name:       "blip",
		Path:       "/bin/sh",
		Args:       []string{"-c", "exit 1"}, // exits immediately
		MinBackoff: 10 * time.Millisecond,
		MaxBackoff: 20 * time.Millisecond,
		StartSecs:  time.Hour, // never "healthy" → always a fast crash
	}
	m := New(p)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx timeout")
	}
	if got := m.Snapshot()[0].Starts; got < 2 {
		t.Fatalf("starts = %d, want >= 2 (program should have restarted)", got)
	}
}

// TestManager_StopsOnCancel: a long-running program is signaled + reaped when
// ctx is canceled, and Run returns promptly (well under the child's sleep).
func TestManager_StopsOnCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test child uses /bin/sh")
	}
	p := &Program{Name: "sleeper", Path: "/bin/sh", Args: []string{"-c", "sleep 30"}}
	m := New(p)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for !m.Snapshot()[0].Running {
		select {
		case <-deadline:
			t.Fatal("program never reported running")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return promptly after cancel (child not stopped?)")
	}
}

// Restart backoff is now decorrelated jitter (internal/jitter), exercised by
// that package's own tests; the supervisor just feeds min/max bounds into it.
