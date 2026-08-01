package runtime

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The whole point of A1: an apiserver that is dead while the container stays Up
// must be recreated. `podman ps` Up is not evidence of a serving control plane,
// so the supervisor gates on the readiness probe, and a sustained dead
// apiserver has to force a recreate — not sit there forever like the one-shot
// UpServer did.
func TestServerSupervisor_RecreatesWhenApiserverDeadPastGrace(t *testing.T) {
	var mu sync.Mutex
	var forced []bool // ForceRecreate flag captured per up() call

	s := &serverSupervisor{
		opts: ServerOptions{AgentName: "host-a"},
		cfg: SupervisorConfig{
			CheckInterval:  time.Millisecond,
			UnhealthyGrace: 5 * time.Millisecond,
			FirstBackoff:   time.Millisecond,
			MaxBackoff:     time.Millisecond,
		},
		up: func(_ context.Context, o ServerOptions) error {
			mu.Lock()
			forced = append(forced, o.ForceRecreate)
			mu.Unlock()
			return nil
		},
		// Container is Up but the apiserver never answers — the exact failure
		// mode the probe exists to catch.
		check: func(_ context.Context, _ ServerOptions) ServerHealth {
			return ServerHealth{Serving: false, ContainerRunning: true, Err: "connection refused"}
		},
		now: time.Now,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = s.run(ctx); close(done) }()

	recreated := func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, f := range forced {
			if f {
				return true
			}
		}
		return false
	}

	deadline := time.After(2 * time.Second)
	for !recreated() {
		select {
		case <-deadline:
			t.Fatal("supervisor never recreated the container despite a sustained dead apiserver")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done

	// The FIRST up() is the initial bring-up and must NOT be a force-recreate —
	// otherwise every boot needlessly destroys and rebuilds a healthy container.
	mu.Lock()
	defer mu.Unlock()
	if len(forced) == 0 || forced[0] {
		t.Fatalf("first bring-up should be non-forced; forced=%v", forced)
	}
}

// The counterpart guard: a serving apiserver must never trigger a recreate. A
// hair-trigger supervisor that restarts a healthy control plane is its own
// outage.
func TestServerSupervisor_DoesNotRecreateWhileServing(t *testing.T) {
	var mu sync.Mutex
	var forced []bool

	s := &serverSupervisor{
		opts: ServerOptions{AgentName: "host-a"},
		cfg: SupervisorConfig{
			CheckInterval:  time.Millisecond,
			UnhealthyGrace: 2 * time.Millisecond,
			FirstBackoff:   time.Millisecond,
			MaxBackoff:     time.Millisecond,
		},
		up: func(_ context.Context, o ServerOptions) error {
			mu.Lock()
			forced = append(forced, o.ForceRecreate)
			mu.Unlock()
			return nil
		},
		check: func(_ context.Context, _ ServerOptions) ServerHealth {
			return ServerHealth{Serving: true, Status: 200, ContainerRunning: true}
		},
		now: time.Now,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.run(ctx); close(done) }()
	time.Sleep(60 * time.Millisecond) // many probe cycles at 1ms
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	for i, f := range forced {
		if f {
			t.Fatalf("recreated a serving control plane at up() #%d: %v", i, forced)
		}
	}
	if len(forced) == 0 {
		t.Fatal("supervisor never brought the container up at all")
	}
}

// A cold container engine at boot must not disable the plane until the next
// daemon restart (the one-shot regression). The supervisor retries; when up()
// starts succeeding, monitoring begins.
func TestServerSupervisor_RetriesBringUpUntilItSucceeds(t *testing.T) {
	var mu sync.Mutex
	attempts := 0

	s := &serverSupervisor{
		opts: ServerOptions{AgentName: "host-a"},
		cfg: SupervisorConfig{
			CheckInterval:  time.Millisecond,
			UnhealthyGrace: time.Hour, // never recreate during this test
			FirstBackoff:   time.Millisecond,
			MaxBackoff:     time.Millisecond,
		},
		up: func(_ context.Context, _ ServerOptions) error {
			mu.Lock()
			attempts++
			n := attempts
			mu.Unlock()
			if n < 3 {
				return context.DeadlineExceeded // stand-in for "engine not ready"
			}
			return nil
		},
		check: func(_ context.Context, _ ServerOptions) ServerHealth {
			return ServerHealth{Serving: true, Status: 200, ContainerRunning: true}
		},
		now: time.Now,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.run(ctx); close(done) }()

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := attempts
		mu.Unlock()
		if n >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("supervisor gave up after %d bring-up attempts; a cold engine must be retried", n)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}

// ctx cancellation must stop the supervisor promptly so a self-restart never
// hangs on it.
func TestServerSupervisor_StopsOnContextCancel(t *testing.T) {
	s := &serverSupervisor{
		opts:  ServerOptions{AgentName: "host-a"},
		cfg:   SupervisorConfig{}.withDefaults(), // production durations
		up:    func(_ context.Context, _ ServerOptions) error { return nil },
		check: func(_ context.Context, _ ServerOptions) ServerHealth { return ServerHealth{Serving: true} },
		now:   time.Now,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v on cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not stop within 2s of cancel — a self-restart would hang here")
	}
}
