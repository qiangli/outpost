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

// stubPS serves the same body on every /api/ps call. nil means "404
// here," which exercises the watcher's graceful-degrade path.
type stubPS struct {
	body  string
	calls atomic.Int32
}

func (s *stubPS) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.calls.Add(1)
	if s == nil || s.body == "" {
		http.NotFound(w, nil)
		return
	}
	_, _ = io.WriteString(w, s.body)
}

// newTestWatcher wires a Watcher to two httptest servers — one for the
// Ollama daemon side, one for the cloudbox registry side. Short poll
// intervals so tests don't drag. ps may be nil to leave /api/ps
// returning 404 (which exercises the watcher's graceful-degrade path
// — the cache stays stale rather than being wiped).
func newTestWatcher(t *testing.T, tags *stubTags, reg *capturingRegistry, ps *stubPS) (*Watcher, *httptest.Server, *httptest.Server) {
	t.Helper()
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			tags.ServeHTTP(w, r)
			return
		case "/api/ps":
			if ps == nil {
				http.NotFound(w, r)
				return
			}
			ps.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
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
	w, _, _ := newTestWatcher(t, tags, reg, nil)

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
	w, _, _ := newTestWatcher(t, tags, reg, nil)

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
	w, _, _ := newTestWatcher(t, tags, reg, nil)

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
	w, _, _ := newTestWatcher(t, tags, reg, nil)
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
	w, _, _ := newTestWatcher(t, tags, reg, nil)

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

func TestWatcher_Status_TracksPushAndError(t *testing.T) {
	tags := &stubTags{bodies: []string{`{"models":[{"name":"a"}]}`}}
	reg := &capturingRegistry{}
	w, _, _ := newTestWatcher(t, tags, reg, nil)

	// Pre-Run: Status should be zero-valued (not running).
	pre := w.Status()
	if pre.Running || pre.PushCount != 0 || !pre.LastPushAt.IsZero() {
		t.Fatalf("pre-Run status not zero: %+v", pre)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	post := w.Status()
	if post.Running {
		t.Errorf("post-Run Running=true (should clear when Run exits)")
	}
	if post.PushCount < 1 {
		t.Errorf("PushCount=%d, want >=1", post.PushCount)
	}
	if post.LastModels != 1 {
		t.Errorf("LastModels=%d, want 1", post.LastModels)
	}
	if post.LastPushAt.IsZero() {
		t.Errorf("LastPushAt should be set after a successful push")
	}
	if post.LastError != "" {
		t.Errorf("LastError=%q, want empty after successful push", post.LastError)
	}
	if post.CloudboxURL == "" || post.OllamaURL == "" {
		t.Errorf("Status URLs missing: cloudbox=%q ollama=%q", post.CloudboxURL, post.OllamaURL)
	}
}

func TestWatcher_Status_RecordsErrorOnFailure(t *testing.T) {
	tags := &stubTags{bodies: []string{`{"models":[]}`}}
	reg := &capturingRegistry{}
	reg.status.Store(int32(http.StatusInternalServerError))
	w, _, _ := newTestWatcher(t, tags, reg, nil)
	w.cfg.PollInterval = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	post := w.Status()
	if post.LastError == "" {
		t.Errorf("LastError should be set after a 5xx push")
	}
	if post.PushCount != 0 {
		t.Errorf("PushCount=%d, want 0 (push never succeeded)", post.PushCount)
	}
}

func TestWatcher_FetchModels_EnrichesFromShow(t *testing.T) {
	tagsBody := `{"models":[{"name":"llama3.2:1b","digest":"d1","details":{"family":"llama","parameter_size":"1B","quantization_level":"Q4"}}]}`
	// Track how many times /api/show was called per (model name) so we
	// can assert per-digest caching.
	var showCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, tagsBody)
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, _ *http.Request) {
		showCalls.Add(1)
		_, _ = io.WriteString(w, `{
		  "capabilities": ["completion","tools"],
		  "model_info": {
		    "general.architecture": "llama",
		    "llama.context_length": 8192
		  }
		}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(cloudSrv.Close)

	w, err := New(Config{
		AgentName:         "test",
		OllamaURL:         srv.URL,
		CloudboxURL:       cloudSrv.URL,
		AccessToken:       "tk",
		PollInterval:      1 * time.Hour,
		HeartbeatInterval: 1 * time.Hour,
		HTTPClient:        srv.Client(),
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// First fetchModels: should call /api/show once for the new digest.
	models, err := w.fetchModels(context.Background())
	if err != nil {
		t.Fatalf("fetchModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("len=%d, want 1", len(models))
	}
	if got := models[0].Capabilities; len(got) != 2 || got[0] != "completion" || got[1] != "tools" {
		t.Errorf("capabilities=%v", got)
	}
	if models[0].ContextLength != 8192 {
		t.Errorf("ContextLength=%d, want 8192", models[0].ContextLength)
	}
	if n := showCalls.Load(); n != 1 {
		t.Errorf("show calls after first fetch=%d, want 1", n)
	}

	// Second fetchModels: same digest → cache hit → no new /api/show calls.
	_, _ = w.fetchModels(context.Background())
	_, _ = w.fetchModels(context.Background())
	if n := showCalls.Load(); n != 1 {
		t.Errorf("show calls after 3 fetches (same digest)=%d, want 1 (cache hit)", n)
	}
}

func TestWatcher_FetchModels_ShowFailureIsCachedAsZero(t *testing.T) {
	tagsBody := `{"models":[{"name":"broken:1b","digest":"dbad","details":{"family":"x"}}]}`
	var showCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, tagsBody)
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, _ *http.Request) {
		showCalls.Add(1)
		http.Error(w, "broken", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(cloud.Close)

	w, err := New(Config{
		AgentName: "t", OllamaURL: srv.URL, CloudboxURL: cloud.URL,
		AccessToken: "k", PollInterval: time.Hour, HeartbeatInterval: time.Hour,
		HTTPClient: srv.Client(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	models, err := w.fetchModels(context.Background())
	if err != nil {
		t.Fatalf("fetchModels: %v", err)
	}
	if len(models[0].Capabilities) != 0 || models[0].ContextLength != 0 {
		t.Errorf("expected zero details on show failure, got caps=%v ctx=%d", models[0].Capabilities, models[0].ContextLength)
	}
	// Second call must NOT re-probe — failure is cached.
	_, _ = w.fetchModels(context.Background())
	if n := showCalls.Load(); n != 1 {
		t.Errorf("show calls after retry=%d, want 1 (failure cached)", n)
	}
}

func TestWatcher_PushIncludesCapacity(t *testing.T) {
	tags := &stubTags{bodies: []string{`{"models":[]}`}}
	reg := &capturingRegistry{}
	w, _, _ := newTestWatcher(t, tags, reg, nil)

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

// stubClusterSource is a fixed ClusterSource for the cluster-push test.
type stubClusterSource struct{ c *ClusterCapacity }

func (s stubClusterSource) ClusterSnapshot() *ClusterCapacity { return s.c }

func TestWatcher_PushIncludesCluster(t *testing.T) {
	tags := &stubTags{bodies: []string{`{"models":[]}`}}
	reg := &capturingRegistry{}
	w, _, _ := newTestWatcher(t, tags, reg, nil)
	w.cfg.Cluster = stubClusterSource{&ClusterCapacity{
		MaxModelBytes: 42 << 30,
		MemberCount:   3,
		Backend:       "gpustack",
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	last, ok := reg.lastPayload()
	if !ok {
		t.Fatal("no payload")
	}
	if last.Cluster == nil {
		t.Fatal("Cluster substruct missing from push")
	}
	if last.Cluster.MemberCount != 3 || last.Cluster.Backend != "gpustack" || last.Cluster.MaxModelBytes != 42<<30 {
		t.Errorf("Cluster=%+v, want {42GiB,3,gpustack}", *last.Cluster)
	}
	// The cluster tag must change the content hash off the model-only
	// value so a membership change re-triggers a full cloudbox Replace.
	if last.ContentHash == ContentHash(nil) {
		t.Errorf("content hash should differ from the model-only hash when a cluster is attached")
	}
}

// A nil ClusterSource (single-machine outpost) must omit the substruct
// and leave the content hash byte-identical to the pre-cluster shape.
func TestWatcher_NoClusterIsSingleMachineShape(t *testing.T) {
	tags := &stubTags{bodies: []string{`{"models":[]}`}}
	reg := &capturingRegistry{}
	w, _, _ := newTestWatcher(t, tags, reg, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	last, ok := reg.lastPayload()
	if !ok {
		t.Fatal("no payload")
	}
	if last.Cluster != nil {
		t.Errorf("Cluster should be nil on a single-machine outpost, got %+v", *last.Cluster)
	}
	if last.ContentHash != ContentHash(nil) {
		t.Errorf("content hash must equal the model-only hash with no cluster attached")
	}
}

func TestWatcher_LoadedSnapshot_FromPS(t *testing.T) {
	tags := &stubTags{bodies: []string{`{"models":[]}`}}
	reg := &capturingRegistry{}
	ps := &stubPS{body: `{"models":[
		{"name":"qwen2.5-coder:7b","digest":"d1"},
		{"name":"llama3.2:3b","digest":"d2"}
	]}`}
	w, _, _ := newTestWatcher(t, tags, reg, ps)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	if ps.calls.Load() == 0 {
		t.Fatal("/api/ps was never polled")
	}
	models, swapping := w.LoadedSnapshot()
	if swapping {
		t.Errorf("Swapping=true, want false (no state/expires_at hints in fixture)")
	}
	want := []string{"llama3.2:3b", "qwen2.5-coder:7b"} // sorted
	if len(models) != len(want) {
		t.Fatalf("LoadedSnapshot models=%v, want %v", models, want)
	}
	for i, m := range models {
		if m != want[i] {
			t.Errorf("models[%d]=%q, want %q", i, m, want[i])
		}
	}
}

func TestWatcher_LoadedSnapshot_DetectsSwapping(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{
			name: "state-loading",
			body: `{"models":[{"name":"a","digest":"d","state":"loading"}]}`,
		},
		{
			name: "state-pulling",
			body: `{"models":[{"name":"a","digest":"d","state":"pulling"}]}`,
		},
		{
			name: "expires-at-in-past",
			body: `{"models":[{"name":"a","digest":"d","expires_at":"2000-01-01T00:00:00Z"}]}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tags := &stubTags{bodies: []string{`{"models":[]}`}}
			reg := &capturingRegistry{}
			ps := &stubPS{body: tt.body}
			w, _, _ := newTestWatcher(t, tags, reg, ps)

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_ = w.Run(ctx)

			_, swapping := w.LoadedSnapshot()
			if !swapping {
				t.Errorf("Swapping=false, want true (fixture: %s)", tt.name)
			}
		})
	}
}

func TestWatcher_LoadedSnapshot_PSFailureLeavesCacheStale(t *testing.T) {
	tags := &stubTags{bodies: []string{`{"models":[]}`}}
	reg := &capturingRegistry{}
	// nil ps ⇒ /api/ps returns 404. Watcher should not panic, should
	// not wipe its cache, should still push tags normally.
	w, _, _ := newTestWatcher(t, tags, reg, nil)

	// Pre-seed the cache so we can prove a failed probe does NOT clear it.
	w.setLoaded([]string{"stale-model"}, false)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	models, swapping := w.LoadedSnapshot()
	if len(models) != 1 || models[0] != "stale-model" || swapping {
		t.Errorf("after failed /api/ps probe: models=%v swapping=%v, want [stale-model] false (stale cache preserved)", models, swapping)
	}
	// Tags push should still have happened.
	if reg.calls.Load() == 0 {
		t.Error("registry push never fired despite /api/ps 404 — tick must not abort on /api/ps failure")
	}
}

func TestWatcher_PushIncludesLoadedAndSwapping_WhenServiceIsCapacitySource(t *testing.T) {
	tags := &stubTags{bodies: []string{`{"models":[]}`}}
	reg := &capturingRegistry{}
	ps := &stubPS{body: `{"models":[{"name":"qwen2.5:0.5b","digest":"d","state":"loading"}]}`}
	w, _, _ := newTestWatcher(t, tags, reg, ps)

	// Wire the service as the capacity source — this is how main.go
	// does it. Service.Snapshot composes counter + watcher loaded
	// cache so the push payload carries the v2 fields.
	svc := NewService(NewCounter())
	svc.SetWatcher(w)
	w.cfg.Capacity = svc

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	last, ok := reg.lastPayload()
	if !ok {
		t.Fatal("no payload")
	}
	if last.Capacity.Version != 2 {
		t.Errorf("Capacity.Version=%d, want 2", last.Capacity.Version)
	}
	if !last.Capacity.Swapping {
		t.Error("Capacity.Swapping=false, want true (fixture state=loading)")
	}
	if len(last.Capacity.LoadedModels) != 1 || last.Capacity.LoadedModels[0] != "qwen2.5:0.5b" {
		t.Errorf("Capacity.LoadedModels=%v, want [qwen2.5:0.5b]", last.Capacity.LoadedModels)
	}
}

// TestContentHash_StableUnderTimestampJitter pins the load-bearing
// invariant of the delta protocol: two pushes whose only difference is
// the ModifiedAt timestamp MUST hash to the same value. Pre-delta the
// watcher used reflect.DeepEqual including ModifiedAt, which fired
// spurious "change" pushes every poll due to Ollama's filesystem-stat
// jitter — exactly the noise floor this commit is meant to fix.
func TestContentHash_StableUnderTimestampJitter(t *testing.T) {
	t1 := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(43 * time.Millisecond)
	a := []ModelInfo{
		{Name: "llama3.2:1b", Digest: "d1", Size: 100, ModifiedAt: t1, Family: "llama", ParameterSize: "1B"},
		{Name: "mistral:7b", Digest: "d2", Size: 200, ModifiedAt: t1, Family: "mistral", ParameterSize: "7B"},
	}
	b := []ModelInfo{
		{Name: "llama3.2:1b", Digest: "d1", Size: 100, ModifiedAt: t2, Family: "llama", ParameterSize: "1B"},
		{Name: "mistral:7b", Digest: "d2", Size: 200, ModifiedAt: t2, Family: "mistral", ParameterSize: "7B"},
	}
	ha, hb := ContentHash(a), ContentHash(b)
	if ha != hb {
		t.Fatalf("timestamp-only difference must not change hash:\n  a=%s\n  b=%s", ha, hb)
	}
	if ha == "" {
		t.Error("ContentHash should be non-empty for non-empty input")
	}
}

// TestContentHash_DiffersOnRealChange flips every stable field one at
// a time and asserts the hash moves. Baseline is the same set used in
// the jitter test so the two together pin both directions.
func TestContentHash_DiffersOnRealChange(t *testing.T) {
	base := []ModelInfo{
		{Name: "llama3.2:1b", Digest: "d1", Size: 100, Family: "llama",
			ParameterSize: "1B", Quantization: "Q4", Capabilities: []string{"completion"}, ContextLength: 4096},
	}
	baseline := ContentHash(base)

	mutators := map[string]func([]ModelInfo){
		"name":           func(m []ModelInfo) { m[0].Name = "llama3.2:3b" },
		"digest":         func(m []ModelInfo) { m[0].Digest = "different" },
		"size":           func(m []ModelInfo) { m[0].Size = 999 },
		"family":         func(m []ModelInfo) { m[0].Family = "qwen" },
		"parameter_size": func(m []ModelInfo) { m[0].ParameterSize = "3B" },
		"quantization":   func(m []ModelInfo) { m[0].Quantization = "Q8" },
		"capabilities":   func(m []ModelInfo) { m[0].Capabilities = []string{"completion", "embedding"} },
		"context_length": func(m []ModelInfo) { m[0].ContextLength = 8192 },
		"add_row":        func(m []ModelInfo) { /* handled inline */ },
	}
	for name, mut := range mutators {
		t.Run(name, func(t *testing.T) {
			cp := make([]ModelInfo, len(base))
			copy(cp, base)
			if name == "add_row" {
				cp = append(cp, ModelInfo{Name: "qwen:7b", Digest: "d2"})
			} else {
				mut(cp)
			}
			if got := ContentHash(cp); got == baseline {
				t.Errorf("changing %q should change hash, got equal: %s", name, got)
			}
		})
	}
}

// TestContentHash_OrderInsensitive: same models posted in different
// order from /api/tags must hash to the same value. Sorting by Name
// is the canonicalization step.
func TestContentHash_OrderInsensitive(t *testing.T) {
	a := []ModelInfo{
		{Name: "a", Digest: "d1"},
		{Name: "b", Digest: "d2"},
	}
	b := []ModelInfo{
		{Name: "b", Digest: "d2"},
		{Name: "a", Digest: "d1"},
	}
	if ContentHash(a) != ContentHash(b) {
		t.Fatal("hash must be order-insensitive")
	}
}

// TestContentHash_CapabilityOrderInsensitive: Ollama may emit
// capabilities in different orders across versions; the hash must not
// flip on the order alone.
func TestContentHash_CapabilityOrderInsensitive(t *testing.T) {
	a := []ModelInfo{{Name: "x", Capabilities: []string{"completion", "embedding"}}}
	b := []ModelInfo{{Name: "x", Capabilities: []string{"embedding", "completion"}}}
	if ContentHash(a) != ContentHash(b) {
		t.Fatal("capability order must not affect hash")
	}
}

// TestWatcher_PushIncludesContentHash verifies the field reaches
// cloudbox on every push. Smoke check tying ContentHash() to the wire.
func TestWatcher_PushIncludesContentHash(t *testing.T) {
	tags := &stubTags{bodies: []string{
		`{"models":[{"name":"a","digest":"d1","size":100}]}`,
	}}
	reg := &capturingRegistry{}
	w, _, _ := newTestWatcher(t, tags, reg, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	last, ok := reg.lastPayload()
	if !ok {
		t.Fatal("no push captured")
	}
	if last.ContentHash == "" {
		t.Fatal("push must carry ContentHash")
	}
	// And the hash must match what ContentHash would compute over the
	// payload's own models slice (proves the field isn't randomly
	// stamped).
	want := ContentHash(last.Models)
	if last.ContentHash != want {
		t.Errorf("ContentHash on wire = %s, computed = %s", last.ContentHash, want)
	}
}

// TestWatcher_TimestampJitterDoesNotPush: this is the integration
// proof that pre-delta we would have pushed on every poll, and now we
// don't. Ollama serves the same model with ModifiedAt advancing by a
// few milliseconds each poll (the realistic noise floor).
func TestWatcher_TimestampJitterDoesNotPush(t *testing.T) {
	bodies := []string{
		`{"models":[{"name":"a","digest":"d","modified_at":"2026-06-07T12:00:00.001Z"}]}`,
		`{"models":[{"name":"a","digest":"d","modified_at":"2026-06-07T12:00:00.044Z"}]}`,
		`{"models":[{"name":"a","digest":"d","modified_at":"2026-06-07T12:00:00.103Z"}]}`,
		`{"models":[{"name":"a","digest":"d","modified_at":"2026-06-07T12:00:00.207Z"}]}`,
		`{"models":[{"name":"a","digest":"d","modified_at":"2026-06-07T12:00:00.311Z"}]}`,
	}
	tags := &stubTags{bodies: bodies}
	reg := &capturingRegistry{}
	w, _, _ := newTestWatcher(t, tags, reg, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	// Initial push is unconditional (1). All subsequent polls only see
	// timestamp jitter — must NOT push.
	if got := reg.calls.Load(); got != 1 {
		t.Errorf("registry calls=%d, want exactly 1 — pre-delta timestamp jitter would have pushed %d times here",
			got, len(bodies))
	}
}
