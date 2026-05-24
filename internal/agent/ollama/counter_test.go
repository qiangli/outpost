package ollama

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
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
