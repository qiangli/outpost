package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCounter_Defaults(t *testing.T) {
	t.Setenv("OLLAMA_NUM_PARALLEL", "")
	c := NewCounter()
	got := c.Snapshot()
	if got.MaxParallel != defaultMaxParallel {
		t.Errorf("MaxParallel=%d, want %d", got.MaxParallel, defaultMaxParallel)
	}
	if got.InFlight != 0 {
		t.Errorf("fresh Counter InFlight=%d, want 0", got.InFlight)
	}
}

func TestCounter_HonorsEnv(t *testing.T) {
	t.Setenv("OLLAMA_NUM_PARALLEL", "7")
	c := NewCounter()
	if got := c.Snapshot().MaxParallel; got != 7 {
		t.Errorf("MaxParallel=%d, want 7", got)
	}
}

func TestCounter_IgnoresBadEnv(t *testing.T) {
	t.Setenv("OLLAMA_NUM_PARALLEL", "abc")
	c := NewCounter()
	if got := c.Snapshot().MaxParallel; got != defaultMaxParallel {
		t.Errorf("MaxParallel=%d, want default %d (bad env should be ignored)", got, defaultMaxParallel)
	}
}

func TestCounter_Snapshot_V2Fields(t *testing.T) {
	t.Setenv("OLLAMA_NUM_PARALLEL", "")
	t.Setenv("OLLAMA_MAX_LOADED_MODELS", "")
	t.Setenv("OLLAMA_KEEP_ALIVE", "")
	c := NewCounter()
	rep := c.Snapshot()
	if rep.Version != 2 {
		t.Errorf("Version=%d, want 2", rep.Version)
	}
	if rep.NumLoadedMax != 0 {
		t.Errorf("NumLoadedMax=%d, want 0 (unset env)", rep.NumLoadedMax)
	}
	if rep.KeepAliveS != 0 {
		t.Errorf("KeepAliveS=%d, want 0 (unset env)", rep.KeepAliveS)
	}
	if rep.Queued != 0 {
		t.Errorf("Queued=%d, want 0 (fresh counter)", rep.Queued)
	}
}

func TestCounter_HonorsLoadedAndKeepAliveEnv(t *testing.T) {
	for _, tt := range []struct {
		name           string
		keepAlive      string
		wantKeepAliveS int
	}{
		{name: "duration", keepAlive: "15m", wantKeepAliveS: 900},
		{name: "bare-seconds", keepAlive: "30", wantKeepAliveS: 30},
		{name: "negative-one-sentinel", keepAlive: "-1", wantKeepAliveS: -1},
		{name: "forever-sentinel", keepAlive: "forever", wantKeepAliveS: -1},
		{name: "infinity-sentinel", keepAlive: "Infinity", wantKeepAliveS: -1},
		{name: "compound-duration", keepAlive: "1h30m", wantKeepAliveS: 5400},
		{name: "garbage", keepAlive: "wat", wantKeepAliveS: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OLLAMA_MAX_LOADED_MODELS", "5")
			t.Setenv("OLLAMA_KEEP_ALIVE", tt.keepAlive)
			c := NewCounter()
			rep := c.Snapshot()
			if rep.NumLoadedMax != 5 {
				t.Errorf("NumLoadedMax=%d, want 5", rep.NumLoadedMax)
			}
			if rep.KeepAliveS != tt.wantKeepAliveS {
				t.Errorf("KeepAliveS=%d, want %d", rep.KeepAliveS, tt.wantKeepAliveS)
			}
		})
	}
}

func TestCounter_QueuedApproximation(t *testing.T) {
	// max_parallel=2; pile up 5 concurrent requests; observe queued=3
	// from inside the gated handler.
	t.Setenv("OLLAMA_NUM_PARALLEL", "2")
	c := NewCounter()
	enter := make(chan struct{}, 5)
	release := make(chan struct{})
	h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		enter <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/chat", nil))
		}()
	}
	for range 5 {
		<-enter
	}
	rep := c.Snapshot()
	if rep.InFlight != 5 {
		t.Errorf("InFlight=%d, want 5", rep.InFlight)
	}
	if rep.Queued != 3 {
		t.Errorf("Queued=%d, want 3 (5 in-flight, 2 parallel)", rep.Queued)
	}
	close(release)
	wg.Wait()
	if got := c.Snapshot().Queued; got != 0 {
		t.Errorf("post-drain Queued=%d, want 0", got)
	}
}

func TestIsGenerationPath(t *testing.T) {
	for _, tt := range []struct {
		path string
		want bool
	}{
		{"/api/chat", true},
		{"/api/generate", true},
		{"/api/embed", true},
		{"/api/embeddings", true},
		{"/v1/chat/completions", true},
		{"/v1/completions", true},
		{"/v1/embeddings", true},
		{"/API/CHAT", true},
		{"/api/tags", false},
		{"/api/show", false},
		{"/api/ps", false},
		{"/_pool/capacity", false},
		{"/", false},
	} {
		if got := isGenerationPath(tt.path); got != tt.want {
			t.Errorf("isGenerationPath(%q)=%v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestCounter_Wrap_GenerationPathsIncrement(t *testing.T) {
	c := NewCounter()
	// Observed inside the handler — the increment fires for the
	// duration of ServeHTTP, so a snapshot taken from the upstream
	// handler should see InFlight>=1.
	var observed atomic.Int64
	h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		observed.Store(int64(c.Snapshot().InFlight))
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if observed.Load() != 1 {
		t.Errorf("InFlight during chat=%d, want 1", observed.Load())
	}
	if got := c.Snapshot().InFlight; got != 0 {
		t.Errorf("InFlight after chat=%d, want 0 (decrement on return)", got)
	}
}

func TestCounter_Wrap_NonGenerationPathsSkip(t *testing.T) {
	c := NewCounter()
	var observed atomic.Int64
	h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		observed.Store(int64(c.Snapshot().InFlight))
		w.WriteHeader(http.StatusOK)
	}))
	for _, p := range []string{"/api/tags", "/api/show", "/_pool/capacity", "/healthz"} {
		observed.Store(0)
		r := httptest.NewRequest(http.MethodGet, p, nil)
		h.ServeHTTP(httptest.NewRecorder(), r)
		if observed.Load() != 0 {
			t.Errorf("path %q observed InFlight=%d during ServeHTTP, want 0", p, observed.Load())
		}
	}
}

func TestCounter_Wrap_DecrementOnClientCancel(t *testing.T) {
	// Regression: when Ollama is hung and the client (cloudbox) drops
	// the connection, the inbound request context is canceled. The
	// in-flight counter must release the slot promptly via the
	// context-listener path — even though next.ServeHTTP is still
	// blocked in the (simulated) hung upstream. Without the
	// context-tied release, the counter would stay incremented for as
	// long as Ollama hangs, and cloudbox's pickLeastLoaded would
	// misroute traffic to avoid this outpost.
	c := NewCounter()
	upstreamReleased := make(chan struct{})
	h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// "Hung Ollama" — wait until the test explicitly releases.
		// We do NOT honor r.Context().Done() here so the test can
		// prove the counter drops via the listener, not via ServeHTTP
		// returning.
		<-upstreamReleased
		w.WriteHeader(http.StatusOK)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/api/chat", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	servedDone := make(chan struct{})
	go func() {
		h.ServeHTTP(w, r)
		close(servedDone)
	}()

	// Spin until the handler increment lands. The goroutine scheduler
	// can delay this by a few ms; the polling loop bounds the wait.
	deadline := time.Now().Add(time.Second)
	for c.Snapshot().InFlight == 0 {
		if time.Now().After(deadline) {
			t.Fatal("counter never observed the increment")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Client disconnect: cancel the inbound context.
	cancel()
	// The listener goroutine should drop the slot within a short
	// window. Poll because the schedule isn't synchronous.
	deadline = time.Now().Add(time.Second)
	for c.Snapshot().InFlight != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("counter did not decrement after client cancel; InFlight=%d", c.Snapshot().InFlight)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Release the "Ollama" so the test goroutine can exit cleanly.
	// The defer release() will fire too — sync.Once ensures we don't
	// double-decrement (counter must stay at 0).
	close(upstreamReleased)
	<-servedDone
	if got := c.Snapshot().InFlight; got != 0 {
		t.Errorf("post-release InFlight=%d, want 0 (sync.Once should prevent double decrement)", got)
	}
}

func TestCounter_Wrap_ConcurrentRequests(t *testing.T) {
	c := NewCounter()
	// Gate the handler so concurrent requests stack up inside Wrap.
	// We assert peak InFlight tracks the number of in-flight
	// goroutines that have entered the handler.
	enter := make(chan struct{}, 8)
	release := make(chan struct{})
	h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		enter <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))

	const n = 5
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/generate", nil))
		}()
	}
	for range n {
		<-enter
	}
	if got := c.Snapshot().InFlight; got != n {
		t.Errorf("peak InFlight=%d, want %d", got, n)
	}
	close(release)
	wg.Wait()
	if got := c.Snapshot().InFlight; got != 0 {
		t.Errorf("post-drain InFlight=%d, want 0", got)
	}
}
