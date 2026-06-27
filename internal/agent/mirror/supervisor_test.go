package mirror

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// fakeLinker drives the supervisor's reachability signal under test control.
type fakeLinker struct {
	mu        sync.Mutex
	reachable bool
	opens     int32
	closes    int32
}

func (f *fakeLinker) set(b bool) { f.mu.Lock(); f.reachable = b; f.mu.Unlock() }

func (f *fakeLinker) Reachable(context.Context, string, bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reachable
}

func (f *fakeLinker) Open(context.Context, string) (string, func(), error) {
	atomic.AddInt32(&f.opens, 1)
	return "fakedest", func() { atomic.AddInt32(&f.closes, 1) }, nil
}

// The mobility contract: reachable→resume (engine runs), remote→pause (engine
// stops + forward closes), local again→resume with a NEW engine start (catch-up).
func TestSupervisor_PauseResumeOnReachability(t *testing.T) {
	old := PollInterval
	PollInterval = 10 * time.Millisecond
	defer func() { PollInterval = old }()

	fl := &fakeLinker{reachable: true}
	var running, starts int32
	sup := &Supervisor{
		Link:   fl,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		run: func(ctx context.Context, _, _ string, _ *slog.Logger) error {
			atomic.AddInt32(&starts, 1)
			atomic.StoreInt32(&running, 1)
			<-ctx.Done() // engine lifecycle: block until paused/cancelled
			atomic.StoreInt32(&running, 0)
			return ctx.Err()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sup.Run(ctx, []conf.MirrorJob{{Source: "/x", Service: "webdav"}})
		close(done)
	}()
	defer func() { cancel(); <-done }()

	// reachable → resume
	waitFor(t, func() bool { return atomic.LoadInt32(&running) == 1 }, time.Second, "resume when reachable")
	if got := atomic.LoadInt32(&starts); got != 1 {
		t.Fatalf("starts=%d, want 1", got)
	}

	// becomes remote → pause (engine stops, forward closed)
	fl.set(false)
	waitFor(t, func() bool { return atomic.LoadInt32(&running) == 0 }, time.Second, "pause when remote")
	waitFor(t, func() bool { return atomic.LoadInt32(&fl.closes) >= 1 }, time.Second, "forward closed on pause")

	// local again → resume with a 2nd engine start (the catch-up)
	fl.set(true)
	waitFor(t, func() bool { return atomic.LoadInt32(&running) == 1 }, time.Second, "resume when local again")
	if got := atomic.LoadInt32(&starts); got < 2 {
		t.Fatalf("starts=%d, want ≥2 (a fresh catch-up sync on resume)", got)
	}
}

func waitFor(t *testing.T, cond func() bool, max time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}
