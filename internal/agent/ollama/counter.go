package ollama

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
//
// numLoadedMax and keepAliveS mirror OLLAMA_MAX_LOADED_MODELS and
// OLLAMA_KEEP_ALIVE so cloudbox can reason about per-host model
// packing and warmth without a separate /api/show round-trip. Both are
// read once at construction; the daemon doesn't expose runtime changes
// to either anyway.
type Counter struct {
	inFlight     atomic.Int64
	maxParallel  int
	numLoadedMax int
	keepAliveS   int
	maxQueue     int
}

// NewCounter returns a Counter sized to the local Ollama's parallel
// capacity. When OLLAMA_NUM_PARALLEL is unset or unparseable we fall
// back to defaultMaxParallel, matching the daemon's own default.
// OLLAMA_MAX_LOADED_MODELS, OLLAMA_KEEP_ALIVE and OLLAMA_MAX_QUEUE
// are surfaced too; each defaults to zero ("let ollama decide") when
// unset or unparseable, which cloudbox must not interpret as "no
// models can load" / "unload immediately" / "zero queue depth."
func NewCounter() *Counter {
	return &Counter{
		maxParallel:  envMaxParallel(),
		numLoadedMax: envMaxLoadedModels(),
		keepAliveS:   envKeepAliveSeconds(),
		maxQueue:     envMaxQueue(),
	}
}

// Snapshot returns the current load+limit snapshot. Safe to call from
// any goroutine; the InFlight read is atomic and the rest are
// immutable after construction. Queued is best-effort: Ollama doesn't
// expose the real internal queue depth, so we approximate as the
// overflow past max_parallel (zero when not overloaded).
//
// LoadedModels / Swapping are NOT filled here — those come from the
// Watcher's /api/ps cache. Service.Snapshot composes the full v2
// report from Counter + Watcher.
func (c *Counter) Snapshot() CapacityReport {
	inFlight := int(c.inFlight.Load())
	queued := 0
	if inFlight > c.maxParallel {
		queued = inFlight - c.maxParallel
	}
	return CapacityReport{
		Version:      2,
		MaxParallel:  c.maxParallel,
		InFlight:     inFlight,
		Queued:       queued,
		NumLoadedMax: c.numLoadedMax,
		KeepAliveS:   c.keepAliveS,
		MaxQueue:     c.maxQueue,
	}
}

// envMaxQueue reads OLLAMA_MAX_QUEUE. Returns 0 on unset/unparseable —
// cloudbox treats zero as "use ollama's default" (512 at time of
// writing). Negative values clamp to zero.
func envMaxQueue() int {
	v := strings.TrimSpace(os.Getenv("OLLAMA_MAX_QUEUE"))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// envMaxParallel reads OLLAMA_NUM_PARALLEL. Falls back to
// defaultMaxParallel on unset / unparseable / non-positive.
func envMaxParallel() int {
	v := strings.TrimSpace(os.Getenv("OLLAMA_NUM_PARALLEL"))
	if v == "" {
		return defaultMaxParallel
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return defaultMaxParallel
	}
	return n
}

// envMaxLoadedModels reads OLLAMA_MAX_LOADED_MODELS. Returns 0 on unset
// or unparseable — cloudbox treats zero as "use ollama's default", not
// "zero models allowed." Negative values are clamped to zero (ollama
// doesn't use negatives here).
func envMaxLoadedModels() int {
	v := strings.TrimSpace(os.Getenv("OLLAMA_MAX_LOADED_MODELS"))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// envKeepAliveSeconds reads OLLAMA_KEEP_ALIVE. Ollama accepts duration
// strings ("5m", "1h30m"), bare seconds as integers, or the sentinel
// "-1" / "forever" / "infinity" meaning "pin loaded forever." We
// translate to seconds: positive duration ⇒ seconds, sentinels ⇒ -1,
// unset/unparseable ⇒ 0 ("use ollama's default").
func envKeepAliveSeconds() int {
	v := strings.TrimSpace(os.Getenv("OLLAMA_KEEP_ALIVE"))
	if v == "" {
		return 0
	}
	switch strings.ToLower(v) {
	case "-1", "forever", "infinity":
		return -1
	}
	if d, err := time.ParseDuration(v); err == nil {
		s := int(d.Seconds())
		if s < 0 {
			return -1
		}
		return s
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return -1
		}
		return n
	}
	return 0
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
//
// Decrement strategy is "first of two events": whichever of these
// fires first releases the slot —
//  1. handler return (the normal path; ServeHTTP returns once the
//     upstream response has fully streamed),
//  2. the inbound request context being cancelled (cloudbox detects
//     a client disconnect, closes the outbound conn to this outpost,
//     our HTTP server cancels r.Context()).
//
// Going context-driven matters for the leak case: when Ollama is
// hung (OOM, stuck generation, network blip), httputil.ReverseProxy's
// Transport.RoundTrip can block much longer than the inbound
// disconnect — the cloudbox-side capacity reading would then show
// the slot occupied for minutes after the client has moved on, and
// the LLM pool router would dutifully avoid this outpost. The
// sync.Once gates the decrement so the two paths don't double-count.
func (c *Counter) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isGenerationPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		c.inFlight.Add(1)
		var once sync.Once
		release := func() { once.Do(func() { c.inFlight.Add(-1) }) }

		// Listener goroutine: decrement on inbound cancel even if
		// ServeHTTP is still grinding on a stuck Ollama. Capturing
		// r.Context() once is right — Go's HTTP server cancels it on
		// client disconnect (read of CloseNotifier-style event +
		// the conn's own io error), and a fresh context per request
		// means we don't leak across requests.
		done := r.Context().Done()
		go func() {
			<-done
			release()
		}()
		defer release()
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
