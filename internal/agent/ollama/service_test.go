package ollama

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestService_CapacityHandler_ReportsCounterSnapshot(t *testing.T) {
	t.Setenv("OLLAMA_NUM_PARALLEL", "6")
	svc := NewService(NewCounter())

	// Drive the counter to InFlight=2 by occupying it from another
	// goroutine, then probe the capacity endpoint.
	hold := make(chan struct{})
	probe := make(chan struct{})
	upstream := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		probe <- struct{}{}
		<-hold
	})
	wrapped := svc.WrapProxy(upstream)
	for range 2 {
		go wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/chat", nil))
	}
	for range 2 {
		<-probe
	}

	w := httptest.NewRecorder()
	svc.CapacityHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/_pool/capacity", nil))
	if got := w.Code; got != http.StatusOK {
		t.Fatalf("status=%d, want 200", got)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", ct)
	}
	var rep CapacityReport
	if err := json.NewDecoder(w.Body).Decode(&rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep.MaxParallel != 6 {
		t.Errorf("MaxParallel=%d, want 6", rep.MaxParallel)
	}
	if rep.InFlight != 2 {
		t.Errorf("InFlight=%d, want 2", rep.InFlight)
	}
	close(hold)
}

func TestService_CounterAccessor(t *testing.T) {
	c := NewCounter()
	svc := NewService(c)
	if svc.Counter() != c {
		t.Error("Counter() must return the bound counter (shared with the watcher)")
	}
}

func TestService_Snapshot_NilWatcher_FallsThroughToCounter(t *testing.T) {
	t.Setenv("OLLAMA_NUM_PARALLEL", "8")
	svc := NewService(NewCounter())
	// Watcher not set; Snapshot must not panic and must return the
	// counter-only view.
	rep := svc.Snapshot()
	if rep.Version != 2 {
		t.Errorf("Version=%d, want 2", rep.Version)
	}
	if rep.MaxParallel != 8 {
		t.Errorf("MaxParallel=%d, want 8", rep.MaxParallel)
	}
	if len(rep.LoadedModels) != 0 || rep.Swapping {
		t.Errorf("LoadedModels=%v Swapping=%v, want empty/false when watcher nil", rep.LoadedModels, rep.Swapping)
	}
}

func TestService_Snapshot_ComposesCounterAndWatcher(t *testing.T) {
	svc := NewService(NewCounter())
	// Stand up a minimal watcher and seed its loaded cache directly.
	w, err := New(Config{OllamaURL: "http://x", CloudboxURL: "http://y"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.setLoaded([]string{"qwen2.5:0.5b", "llama3.2:3b"}, true)
	svc.SetWatcher(w)

	rep := svc.Snapshot()
	if rep.Version != 2 {
		t.Errorf("Version=%d, want 2", rep.Version)
	}
	if !rep.Swapping {
		t.Error("Swapping=false, want true (overlaid from watcher)")
	}
	if len(rep.LoadedModels) != 2 {
		t.Errorf("LoadedModels=%v, want 2 entries", rep.LoadedModels)
	}
}

func TestService_CapacityHandler_ServesV2Shape(t *testing.T) {
	t.Setenv("OLLAMA_NUM_PARALLEL", "4")
	t.Setenv("OLLAMA_MAX_LOADED_MODELS", "3")
	t.Setenv("OLLAMA_KEEP_ALIVE", "15m")
	svc := NewService(NewCounter())
	w, err := New(Config{OllamaURL: "http://x", CloudboxURL: "http://y"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.setLoaded([]string{"a", "b"}, false)
	svc.SetWatcher(w)

	rr := httptest.NewRecorder()
	svc.CapacityHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_pool/capacity", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var rep CapacityReport
	if err := json.NewDecoder(rr.Body).Decode(&rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep.Version != 2 {
		t.Errorf("Version=%d, want 2", rep.Version)
	}
	if rep.NumLoadedMax != 3 {
		t.Errorf("NumLoadedMax=%d, want 3", rep.NumLoadedMax)
	}
	if rep.KeepAliveS != 900 {
		t.Errorf("KeepAliveS=%d, want 900", rep.KeepAliveS)
	}
	if len(rep.LoadedModels) != 2 {
		t.Errorf("LoadedModels=%v, want 2 entries", rep.LoadedModels)
	}
}
