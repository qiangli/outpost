package ollama

import (
	"encoding/json"
	"net/http"
)

// Service glues a Counter to the two outpost-side surfaces it feeds:
// the proxy-wrap middleware (for live in-flight tracking) and the
// /_pool/capacity intercept handler (for cloudbox-side scheduling
// queries). One Service per Ollama mount; the Counter is shared with
// the Watcher so all three signals come from the same source.
type Service struct {
	counter *Counter
}

// NewService binds a Service to counter. counter must be non-nil.
func NewService(counter *Counter) *Service {
	return &Service{counter: counter}
}

// Counter returns the embedded counter so the Watcher can read live
// capacity from the same source the proxy middleware writes to.
func (s *Service) Counter() *Counter { return s.counter }

// WrapProxy is the middleware factory passed to
// AppRegistry.SetProxyWrap. Every request proxied to the local Ollama
// daemon flows through Counter.Wrap, which increments the in-flight
// gauge only on generation paths.
func (s *Service) WrapProxy(next http.Handler) http.Handler {
	return s.counter.Wrap(next)
}

// CapacityHandler returns the http.Handler bound at
// /app/ollama/_pool/capacity. It returns the live CapacityReport as
// JSON. No body input; the snapshot is read directly from the
// counter.
//
// The endpoint must answer quickly — cloudbox's scheduler may probe
// it on every request — so we encode directly into the response
// without holding any locks beyond the atomic load inside
// Counter.Snapshot.
func (s *Service) CapacityHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.counter.Snapshot())
	})
}
