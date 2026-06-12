package sandbox

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// Counter tracks in-flight sandbox create/exec requests proxied through
// this outpost's /app/sandbox/* route. It is the load signal cloudbox's
// router reads (via CapacityReport.InFlight) to pick the least-loaded host
// when distributing a sandbox request across the fleet — the container
// analog of ollama.Counter.
//
// Only the request shapes that actually spin up work count: container
// create and exec create. Listing / inspect / pulls are free, the same
// way ollama's /api/tags pulls don't burn a generation slot.
type Counter struct {
	inFlight      atomic.Int64
	maxContainers int
}

// NewCounter returns a Counter whose advertised ceiling is maxContainers
// (0 == unset).
func NewCounter(maxContainers int) *Counter {
	return &Counter{maxContainers: maxContainers}
}

// Snapshot returns the live capacity report sans pool fields (the Service
// overlays those). InFlight is an atomic load; MaxContainers is immutable
// after construction.
func (c *Counter) Snapshot() CapacityReport {
	return CapacityReport{
		Version:       1,
		MaxContainers: c.maxContainers,
		InFlight:      int(c.inFlight.Load()),
	}
}

// Wrap returns next instrumented so requests that spin up work bump the
// in-flight gauge for the duration of the request. Decrement is "first of
// two events" — handler return OR inbound-context cancel — so a hung
// daemon doesn't pin the slot past a client disconnect, mirroring the
// ollama counter's leak-defense.
func (c *Counter) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWorkPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		c.inFlight.Add(1)
		var once sync.Once
		release := func() { once.Do(func() { c.inFlight.Add(-1) }) }
		done := r.Context().Done()
		go func() {
			<-done
			release()
		}()
		defer release()
		next.ServeHTTP(w, r)
	})
}

// isWorkPath reports whether a request against the sandbox mount actually
// spins up work (container create or exec create). Matches both docker-
// compat and libpod path shapes after the /app/sandbox prefix strip.
func isWorkPath(p string) bool {
	return isContainerCreatePath(p) || isExecCreatePath(p) ||
		strings.HasSuffix(strings.ToLower(strings.TrimRight(p, "/")), "/start")
}
