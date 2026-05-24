package ollama

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

// defaultMaxParallel matches Ollama's own default for OLLAMA_NUM_PARALLEL
// as of 0.5+. Cloudbox's scheduler reads this through CapacityReport so
// it never schedules more than the daemon will actually serve at once.
const defaultMaxParallel = 4

// Counter tracks in-flight LLM requests proxied through this outpost's
// /app/ollama/* route. Atomic; safe for concurrent use.
//
// The counter is intentionally Ollama-shaped (not generic) because the
// pool scheduler in cloudbox makes decisions per-model and per-host: it
// needs to know "how many slots are this Ollama burning right now", not
// "how many HTTP requests are in flight." That's why Wrap only
// increments for the request paths that actually consume a generation
// slot — /api/tags or /api/show pulls are free.
type Counter struct {
	inFlight    atomic.Int64
	maxParallel int
}

// NewCounter returns a Counter sized to the local Ollama's parallel
// capacity. When OLLAMA_NUM_PARALLEL is unset or unparseable we fall
// back to defaultMaxParallel, matching the daemon's own default.
func NewCounter() *Counter {
	max := defaultMaxParallel
	if v := strings.TrimSpace(os.Getenv("OLLAMA_NUM_PARALLEL")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			max = n
		}
	}
	return &Counter{maxParallel: max}
}

// Snapshot returns the current load+limit pair. Safe to call from any
// goroutine; the read is atomic for InFlight and immutable for
// MaxParallel.
func (c *Counter) Snapshot() CapacityReport {
	return CapacityReport{
		MaxParallel: c.maxParallel,
		InFlight:    int(c.inFlight.Load()),
	}
}

// Wrap returns next instrumented so calls whose request path consumes
// a generation slot bump the counter for the duration of the request
// (including streaming body reads). Non-generation paths (/api/tags,
// /api/show, /api/ps, capacity probes, health checks) pass through
// without touching the counter.
//
// We instrument at the path level rather than wrapping the response
// body because httputil.ReverseProxy already handles body lifecycle
// for streamed SSE responses — we'd race with its own close hooks.
// Decrementing on handler return is correct because ServeHTTP blocks
// until the upstream response has fully streamed.
func (c *Counter) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isGenerationPath(r.URL.Path) {
			c.inFlight.Add(1)
			defer c.inFlight.Add(-1)
		}
		next.ServeHTTP(w, r)
	})
}

// isGenerationPath reports whether a request path against an Ollama
// daemon will consume a generation slot. Both Ollama's native API
// (/api/chat, /api/generate, /api/embed, /api/embeddings) and its
// OpenAI-compatible API (/v1/chat/completions, /v1/embeddings,
// /v1/completions) count. The path matched is whatever the request
// arrived at the proxy with — by the time Wrap sees it, the
// /app/ollama prefix has already been stripped by AppRegistry.ProxyTo.
//
// Suffix matching tolerates both "/api/generate" and any future
// versioned-subpath shape the daemon adds. Prefix matching on the
// OpenAI surface stops a hostile path like /v1/foo from sneaking in.
func isGenerationPath(p string) bool {
	p = strings.ToLower(p)
	switch {
	case strings.HasSuffix(p, "/api/chat"),
		strings.HasSuffix(p, "/api/generate"),
		strings.HasSuffix(p, "/api/embed"),
		strings.HasSuffix(p, "/api/embeddings"):
		return true
	}
	// OpenAI-compatible endpoints — Ollama mounts these at /v1/...
	// directly, no /api/ prefix. Match against the suffix so the same
	// detection works whether the path arrived as /v1/chat/completions
	// (no /app/<name> strip) or /api/v1/chat/completions (some
	// proxies rewrite). Both cases burn a generation slot.
	for _, sfx := range []string{
		"/v1/chat/completions",
		"/v1/completions",
		"/v1/embeddings",
	} {
		if strings.HasSuffix(p, sfx) {
			return true
		}
	}
	return false
}
