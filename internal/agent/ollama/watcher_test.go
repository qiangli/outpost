package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubTags serves a sequence of /api/tags responses. Each call returns
// the next entry in `bodies`, then sticks on the last entry.
type stubTags struct {
	mu     sync.Mutex
	bodies []string
	idx    int
	calls  atomic.Int32
}

func (s *stubTags) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	body := s.bodies[s.idx]
	if s.idx < len(s.bodies)-1 {
		s.idx++
	}
	_, _ = io.WriteString(w, body)
}

// capturingRegistry records every push to /api/v1/llm/registry.
type capturingRegistry struct {
	mu       sync.Mutex
	payloads []RegistryPushPayload
	calls    atomic.Int32
	// status overrides the response (default 200). Used to assert auth
	// and backoff behavior.
	status atomic.Int32
	auths  []string
}

func (cr *capturingRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cr.calls.Add(1)
	cr.mu.Lock()
	cr.auths = append(cr.auths, r.Header.Get("Authorization"))
	var p RegistryPushPayload
	_ = json.NewDecoder(r.Body).Decode(&p)
	cr.payloads = append(cr.payloads, p)
	cr.mu.Unlock()
	st := int(cr.status.Load())
	if st == 0 {
		st = http.StatusOK
	}
	w.WriteHeader(st)
}

func (cr *capturingRegistry) lastPayload() (RegistryPushPayload, bool) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if len(cr.payloads) == 0 {
		return RegistryPushPayload{}, false
	}
	return cr.payloads[len(cr.payloads)-1], true
}

// newTestWatcher wires a Watcher to two httptest servers — one for the
// Ollama daemon side, one for the cloudbox registry side. Short poll
// intervals so tests don't drag.
func newTestWatcher(t *testing.T, tags *stubTags, reg *capturingRegistry) (*Watcher, *httptest.Server, *httptest.Server) {
	t.Helper()
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		tags.ServeHTTP(w, r)
	}))
	t.Cleanup(ollamaSrv.Close)

	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/llm/registry" {
			http.NotFound(w, r)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(cloudSrv.Close)

	w, err := New(Config{
		AgentName:         "test-agent",
		Version:           "abc1234",
		OllamaURL:         ollamaSrv.URL,
		CloudboxURL:       cloudSrv.URL,
		AccessToken:       "TOKEN-XYZ",
		PollInterval:      20 * time.Millisecond,
		HeartbeatInterval: 1 * time.Hour, // suppress heartbeats; tests assert change pushes
		HTTPClient:        cloudSrv.Client(),
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w, ollamaSrv, cloudSrv
}

func TestWatcher_New_Validation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("empty config should error")
	}
	if _, err := New(Config{OllamaURL: "http://x", CloudboxURL: "http://y"}); err != nil {
		t.Errorf("min valid config errored: %v", err)
	}
}

func TestWatcher_InitialPushAndChangeDetection(t *testing.T) {
	tags := &stubTags{bodies: []string{
		`{"models":[{"name":"llama3.2:1b","digest":"d1","size":100,"details":{"family":"llama","parameter_size":"1B","quantization_level":"Q4"}}]}`,
		`{"models":[{"name":"llama3.2:1b","digest":"d1","size":100,"details":{"family":"llama","parameter_size":"1B","quantization_level":"Q4"}}]}`,
		`{"models":[{"name":"llama3.2:1b","digest":"d1","size":100,"details":{"family":"llama","parameter_size":"1B","quantization_level":"Q4"}},{"name":"mistral:7b","digest":"d2","size":200,"details":{"family":"mistral","parameter_size":"7B","quantization_level":"Q5"}}]}`,
	}}
	reg := &capturingRegistry{}
	w, _, _ := newTestWatcher(t, tags, reg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Wait for at least 2 pushes (initial + change). Bail at 2 s to keep
	// CI quick if logic is broken.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reg.calls.Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if got := reg.calls.Load(); got < 2 {
		t.Fatalf("registry calls=%d, want >=2 (initial + change)", got)
	}
	if got := reg.calls.Load(); got > 4 {
		t.Errorf("registry calls=%d, want <=4 (no spurious heartbeats during 2s)", got)
	}

	last, ok := reg.lastPayload()
	if !ok {
		t.Fatal("no payloads captured")
	}
	if last.AgentName != "test-agent" {
		t.Errorf("AgentName=%q, want test-agent", last.AgentName)
	}
	if last.Version != "abc1234" {
		t.Errorf("Version=%q, want abc1234", last.Version)
	}
	if len(last.Models) != 2 {
		t.Errorf("len(Models)=%d, want 2", len(last.Models))
	}
	if last.HeartbeatAt.IsZero() {
		t.Error("HeartbeatAt should be set")
	}
	if got := reg.auths[0]; got != "Bearer TOKEN-XYZ" {
		t.Errorf("Authorization=%q, want Bearer TOKEN-XYZ", got)
	}
}

func TestWatcher_SuppressesPushWhenUnchanged(t *testing.T) {
	body := `{"models":[{"name":"a","digest":"d"}]}`
	tags := &stubTags{bodies: []string{body, body, body, body, body}}
	reg := &capturingRegistry{}
	w, _, _ := newTestWatcher(t, tags, reg)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	// Initial push is unconditional; subsequent ticks see no change and
	// no heartbeat (1h interval), so should not push again.
	if got := reg.calls.Load(); got != 1 {
		t.Errorf("registry calls=%d, want exactly 1 (initial only; unchanged should not push)", got)
	}
}

func TestWatcher_HeartbeatFiresWithoutChange(t *testing.T) {
	body := `{"models":[{"name":"a","digest":"d"}]}`
	tags := &stubTags{bodies: []string{body}}
	reg := &capturingRegistry{}

	// Cloud + ollama plumbing identical to newTestWatcher but with a
	// very short heartbeat interval so the second push fires within
	// the test budget.
	ollamaSrv := httptest.NewServer(tags)
	t.Cleanup(ollamaSrv.Close)
	cloudSrv := httptest.NewServer(reg)
	t.Cleanup(cloudSrv.Close)

	w, err := New(Config{
		AgentName:         "hb-agent",
		OllamaURL:         ollamaSrv.URL + "/api/tags",
		CloudboxURL:       cloudSrv.URL + "/api/v1/llm/registry",
		AccessToken:       "tk",
		PollInterval:      20 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond,
		HTTPClient:        cloudSrv.Client(),
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The URLs include the path because we just used the raw test
	// servers, but the watcher appends its own paths. Reconstruct
	// without the path:
	w.cfg.OllamaURL = ollamaSrv.URL
	w.cfg.CloudboxURL = cloudSrv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	if got := reg.calls.Load(); got < 2 {
		t.Errorf("registry calls=%d, want >=2 (initial + heartbeat)", got)
	}
}

func TestWatcher_AuthRevoked_Stops(t *testing.T) {
	tags := &stubTags{bodies: []string{`{"models":[]}`}}
	reg := &capturingRegistry{}
	reg.status.Store(int32(http.StatusUnauthorized))
	w, _, _ := newTestWatcher(t, tags, reg)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	err := w.Run(ctx)
	if !errors.Is(err, ErrAuthRevoked) {
		t.Fatalf("Run err=%v, want ErrAuthRevoked", err)
	}
	if got := reg.calls.Load(); got != 1 {
		t.Errorf("registry calls=%d, want 1 (one push then stop on 401)", got)
	}
}

func TestWatcher_NoAccessTokenIsNoOp(t *testing.T) {
	tags := &stubTags{bodies: []string{`{"models":[]}`}}
	reg := &capturingRegistry{}
	w, _, _ := newTestWatcher(t, tags, reg)
	w.cfg.AccessToken = ""

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run with empty token should return nil, got %v", err)
	}
	if got := reg.calls.Load(); got != 0 {
		t.Errorf("registry calls=%d, want 0 (unpaired outpost must not push)", got)
	}
}

func TestWatcher_BackoffOnFailure(t *testing.T) {
	tags := &stubTags{bodies: []string{`{"models":[]}`}}
	reg := &capturingRegistry{}
	reg.status.Store(int32(http.StatusInternalServerError))
	w, _, _ := newTestWatcher(t, tags, reg)

	// Very short ceiling so the test doesn't drag.
	w.cfg.PollInterval = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	got := reg.calls.Load()
	// With a 5ms poll and a 5s minimum backoff, the watcher should
	// hit the registry once (initial push) and then sit on the
	// backoff for the rest of the 300ms window. That asserts the
	// backoff kicks in (we don't see N hundred calls).
	if got > 3 {
		t.Errorf("registry calls=%d during 300ms with 5xx server, want <=3 (backoff kicked in)", got)
	}
}

func TestWatcher_PushIncludesCapacity(t *testing.T) {
	tags := &stubTags{bodies: []string{`{"models":[]}`}}
	reg := &capturingRegistry{}
	w, _, _ := newTestWatcher(t, tags, reg)

	cap := NewCounter()
	w.cfg.Capacity = cap

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	last, ok := reg.lastPayload()
	if !ok {
		t.Fatal("no payload")
	}
	if last.Capacity.MaxParallel != defaultMaxParallel {
		t.Errorf("Capacity.MaxParallel=%d, want %d", last.Capacity.MaxParallel, defaultMaxParallel)
	}
}
