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
