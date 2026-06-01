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
//
// SetWatcher attaches the Watcher after construction (the watcher is
// built later in main.go because it needs cloudbox URL + access token
// resolved). Status() then combines watcher state + counter snapshot
// into one PoolStatus the admin UI + CLI consume.
type Service struct {
	counter *Counter
	watcher *Watcher
}

// PoolStatus is the unified status read by the admin UI and the CLI.
type PoolStatus struct {
	Capacity CapacityReport `json:"capacity"`
	Watcher  WatcherStatus  `json:"watcher"`
}

// NewService binds a Service to counter. counter must be non-nil.
func NewService(counter *Counter) *Service {
	return &Service{counter: counter}
}

// Counter returns the embedded counter so the Watcher can read live
// capacity from the same source the proxy middleware writes to.
func (s *Service) Counter() *Counter { return s.counter }

// SetWatcher records the watcher reference so Status() can include
// push state. Passing nil is allowed (e.g. when pool is off and the
// watcher was never built); Status() in that case returns just the
// capacity slice with an empty WatcherStatus.
func (s *Service) SetWatcher(w *Watcher) { s.watcher = w }

// Status returns the combined diagnostic snapshot.
func (s *Service) Status() PoolStatus {
	out := PoolStatus{Capacity: s.Snapshot()}
	if s.watcher != nil {
		out.Watcher = s.watcher.Status()
	}
	return out
}

// Snapshot composes the full v2 CapacityReport by overlaying the
// Watcher's /api/ps cache (loaded models + swapping signal) on the
// Counter's counter-known fields. Implements CapacitySource so the
// Watcher's push payload carries the same enriched report cloudbox
// gets from the per-routing /_pool/capacity probe.
//
// Watcher may be nil when the pool is off / the outpost is unpaired;
// in that case we return just the counter snapshot (no loaded info).
func (s *Service) Snapshot() CapacityReport {
	rep := s.counter.Snapshot()
	if s.watcher != nil {
		rep.LoadedModels, rep.Swapping = s.watcher.LoadedSnapshot()
	}
	return rep
}

// WrapProxy is the middleware factory passed to
// AppRegistry.SetProxyWrap. Every request proxied to the local Ollama
// daemon flows through Counter.Wrap, which increments the in-flight
// gauge only on generation paths.
func (s *Service) WrapProxy(next http.Handler) http.Handler {
	return s.counter.Wrap(next)
}

// CapacityHandler returns the http.Handler bound at
// /app/ollama/_pool/capacity. It returns the live v2 CapacityReport
// as JSON. No body input; Snapshot composes from the counter's atomic
// load plus the watcher's cached /api/ps result.
//
// The endpoint must answer quickly — cloudbox's scheduler may probe
// it on every request — so we encode directly into the response. The
// atomic InFlight load and the loadedMu critical section are both
// cheap.
func (s *Service) CapacityHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.Snapshot())
	})
}
