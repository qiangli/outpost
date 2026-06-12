package sandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeLibpod is a minimal stand-in for the libpod image API: it tracks
// which images are "present" and records pulls. /images/<ref>/exists
// returns 204/404; /images/pull marks the ref present and 200s.
type fakeLibpod struct {
	mu      sync.Mutex
	present map[string]bool
	pulls   map[string]int
}

func newFakeLibpod(present ...string) *fakeLibpod {
	f := &fakeLibpod{present: map[string]bool{}, pulls: map[string]int{}}
	for _, p := range present {
		f.present[p] = true
	}
	return f
}

func (f *fakeLibpod) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/exists"):
			ref := strings.TrimSuffix(strings.TrimPrefix(path, libpodPrefix+"/images/"), "/exists")
			f.mu.Lock()
			ok := f.present[ref]
			f.mu.Unlock()
			if ok {
				w.WriteHeader(http.StatusNoContent)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/images/pull"):
			ref := r.URL.Query().Get("reference")
			f.mu.Lock()
			f.pulls[ref]++
			f.present[ref] = true // pull succeeds
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"images":["` + ref + `"]}`))
		default:
			http.NotFound(w, r)
		}
	})
}

func (f *fakeLibpod) pullCount(ref string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pulls[ref]
}

func TestPrewarmer_PullsMissingImages(t *testing.T) {
	fake := newFakeLibpod() // nothing present
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	imgs := []string{"docker.io/library/python:3.12", "alpine:3.21"}
	p := newPrewarmer(srv.Client(), srv.URL, imgs)
	p.reconcile(context.Background())

	if got := p.Ready(); got != 2 {
		t.Fatalf("Ready()=%d, want 2", got)
	}
	for _, img := range imgs {
		if fake.pullCount(img) != 1 {
			t.Errorf("image %q pulled %d times, want 1", img, fake.pullCount(img))
		}
	}
}

func TestPrewarmer_SkipsPresentImages(t *testing.T) {
	fake := newFakeLibpod("alpine:3.21") // already present
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	p := newPrewarmer(srv.Client(), srv.URL, []string{"alpine:3.21", "busybox:latest"})
	p.reconcile(context.Background())

	if got := p.Ready(); got != 2 {
		t.Fatalf("Ready()=%d, want 2", got)
	}
	if c := fake.pullCount("alpine:3.21"); c != 0 {
		t.Errorf("already-present image pulled %d times, want 0", c)
	}
	if c := fake.pullCount("busybox:latest"); c != 1 {
		t.Errorf("missing image pulled %d times, want 1", c)
	}
}

func TestPrewarmer_FailedPullNotCounted(t *testing.T) {
	// A server that 404s exists and 500s pull → image never becomes ready.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/exists") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newPrewarmer(srv.Client(), srv.URL, []string{"nope/bad:img"})
	p.reconcile(context.Background())
	if got := p.Ready(); got != 0 {
		t.Fatalf("Ready()=%d, want 0 (pull failed)", got)
	}
}

func TestPrewarmer_EmptyListRunBlocksUntilCancel(t *testing.T) {
	p := NewPrewarmer("/nonexistent.sock", nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	cancel()
	if err := <-done; err == nil {
		t.Fatal("Run should return ctx error after cancel")
	}
	if p.Total() != 0 {
		t.Errorf("Total()=%d, want 0", p.Total())
	}
}
