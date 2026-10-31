package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrAuthRevoked is returned by Run when cloudbox rejects the watcher's
// access_token (HTTP 401). Pairing has been removed cloud-side; the
// watcher stops permanently and the caller's errgroup learns the
// outpost is no longer trusted to push.
var ErrAuthRevoked = errors.New("ollama watcher: cloudbox rejected access_token (401)")

// Defaults tuned to "frequent enough that a model-pull shows up in the
// cloud within ~minutes" + "rare enough to not spam cloudbox when an
// outpost is idle for hours." Push-on-change is the actual freshness
// signal; the heartbeat just keeps cloudbox's last-seen timestamp warm
// so a quiet outpost doesn't get marked offline.
const (
	defaultPollInterval      = 30 * time.Second
	defaultHeartbeatInterval = 5 * time.Minute
	defaultMinBackoff        = 5 * time.Second
	defaultMaxBackoff        = 5 * time.Minute
	maxInferenceProbeBody    = 64 << 10
)

// CapacitySource is what the watcher calls to populate the capacity
// section of each push. Decoupled from *Counter so tests can plug a
// stub and so the watcher does not own the request-counting handler.
type CapacitySource interface {
	Snapshot() CapacityReport
}

// ClusterSource is what the watcher calls to learn whether an intra-home
// distributed-inference backend (GPUStack first; see ClusterCapacity)
// fronts this host. Returns nil when no cluster is detected — the common
// single-machine case — so the registry push omits the cluster
// substruct. Decoupled from the clusterllm probe the same way
// CapacitySource is decoupled from Counter: main.go wires the concrete
// detector, tests plug a stub, and a nil Config.Cluster disables it.
type ClusterSource interface {
	ClusterSnapshot() *ClusterCapacity
}

// ClusterModelSource is the optional extension for a ClusterSource that can
// also inventory the serving models exposed by the cluster's OpenAI /v1 API.
// Kept separate so older tests/stubs that only care about capacity do not
// need to implement a no-op model method.
type ClusterModelSource interface {
	ClusterModels() []ModelInfo
}

// Config is the wiring the watcher needs at construction time. All
// fields are required except the optional intervals/clients/logger
// which fall back to package defaults.
type Config struct {
	// AgentName identifies this outpost in the push payload (informational;
	// cloudbox trusts the bearer token for the actual identity binding).
	AgentName string

	// Version is the outpost short-commit string so cloudbox can report
	// "this agent is stale" without a separate probe.
	Version string

	// OllamaURL is the base URL of the local Ollama daemon (typically
	// http://127.0.0.1:11434). The watcher appends /api/tags.
	OllamaURL string

	// ExtraOllamaURLs are additional local Ollama daemons whose models
	// join the same pool push — first entry is conventionally bashy's
	// own engine (BashyOllamaURL, 127.0.0.1:11435), which keeps a
	// separate model store from the host daemon. Each extra is probed
	// best-effort per tick: a down extra is skipped, never fatal.
	// Entries equal to OllamaURL (or empty) are dropped by New.
	ExtraOllamaURLs []string

	// CloudboxURL is the base URL of cloudbox (e.g. https://ai.dhnt.io).
	// The watcher appends /api/v1/llm/registry.
	CloudboxURL string

	// AccessToken is the per-outpost bearer JWT (fc.AccessToken). Sent on
	// every push; an empty token disables the watcher (it logs once and
	// returns nil).
	AccessToken string

	// Capacity is optional. When non-nil, each push includes a live
	// CapacityReport from this source so cloudbox can avoid
	// over-scheduling.
	Capacity CapacitySource

	// Cluster is optional. When non-nil and it reports a detected
	// backend, each push includes a ClusterCapacity substruct so
	// cloudbox's tier-0 router can route a too-big-for-one-node model to
	// this home. Nil (or a source that returns nil) keeps the push
	// byte-identical to the single-machine shape.
	Cluster ClusterSource

	// LANEndpoint is the direct same-LAN inference URL to advertise in each
	// push (RegistryPushPayload.LANEndpoint), e.g.
	// "http://192.0.2.10:11436/v1". Empty (the common case) omits the field
	// from the wire — set from the LAN inference unit's bound port, so it
	// is advertised only while that unit is actually serving.
	LANEndpoint string

	// WarmStatus, when set, supplies the host's warm-serving signal folded
	// into each push: bytes is the conservative, load-aware warm-preload
	// budget (0 when the host is busy) and busy is the current busy state
	// (RegistryPushPayload.WarmBudgetBytes / Busy). Closure so the watcher
	// doesn't import the sysload profiler; nil keeps both fields at their
	// zero (omitted) values — the pre-warm-serving shape.
	WarmStatus func() (bytes int64, busy bool)

	// WorkerRequestID marks this host as a provisioned cloud GPU worker
	// (normally WorkerRequestID(), i.e. $OUTPOST_GPU_WORKER_REQUEST_ID).
	// Empty — every ordinary host — keeps the worker block off the wire.
	WorkerRequestID string

	// WorkerGPU measures total VRAM + GPU model for the worker block.
	// Consulted only when WorkerRequestID is set; nil → MeasureNVIDIAGPU.
	// An error omits the block (logged) and is retried on the next tick.
	WorkerGPU func(ctx context.Context) (vramBytes int64, gpuModel string, err error)

	// PollInterval overrides the /api/tags poll cadence. Zero → default.
	PollInterval time.Duration

	// HeartbeatInterval overrides the unconditional-push interval. Zero
	// → default. Set very high to disable heartbeats (model-change
	// pushes still fire).
	HeartbeatInterval time.Duration

	// HTTPClient is used for both the local /api/tags probe and the
	// remote /api/v1/llm/registry push. Nil → http.DefaultClient. Tests
	// inject a client backed by httptest.NewServer.
	HTTPClient *http.Client

	// Logger is the structured logger. Nil → slog.Default().
	Logger *slog.Logger
}

// WatcherStatus is the diagnostic snapshot the admin UI + `outpost pool
// status` CLI render. PushCount is total successful pushes since boot
// (resets on restart); LastError is empty on success and carries the
// last failure reason while the watcher is in backoff. Running is true
// from Run() entry until Run() returns.
type WatcherStatus struct {
	Running     bool      `json:"running"`
	LastPushAt  time.Time `json:"last_push_at,omitzero"`
	LastModels  int       `json:"last_models"`
	PushCount   int64     `json:"push_count"`
	LastError   string    `json:"last_error,omitempty"`
	CloudboxURL string    `json:"cloudbox_url,omitempty"`
	OllamaURL   string    `json:"ollama_url,omitempty"`
}

// Watcher polls the local Ollama daemon's model inventory and publishes
// changes to cloudbox.
type Watcher struct {
	cfg Config

	mu    sync.Mutex
	state WatcherStatus

	// detailsCache memoizes /api/show responses by digest. A model's
	// digest changes when its bytes change, so a cache miss is the
	// right trigger to re-probe. New models pulled at runtime get
	// probed exactly once. Bounded loosely by the number of distinct
	// models ever loaded on this host (tiny in practice).
	detailsMu    sync.Mutex
	detailsCache map[string]modelDetails

	// loadedMu protects the cached /api/ps result. Refreshed on every
	// tick; consumed by LoadedSnapshot for the capacity probe and the
	// registry push.
	loadedMu     sync.Mutex
	loadedModels []string
	swapping     bool

	// worker caches the measured worker block (VRAM does not change under
	// a running host); workerErr is the last measurement failure, kept so
	// a persistent failure is logged once rather than every tick.
	worker    *WorkerInfo
	workerErr string

	// tickMu serializes push cycles: Run's loop and PushNow (the GPU
	// worker watchdog's final push) both drive tick, and tick owns the
	// worker cache above.
	tickMu sync.Mutex
}

// LoadedSnapshot returns the watcher's most recent view of which
// models are loaded on the local Ollama daemon, plus a hint about
// whether a model swap is in progress. Safe to call from any
// goroutine. Returns empty/false when the watcher hasn't yet completed
// its first /api/ps probe — callers should treat that as "we don't
// know yet," not "no models loaded."
func (w *Watcher) LoadedSnapshot() (models []string, swapping bool) {
	w.loadedMu.Lock()
	defer w.loadedMu.Unlock()
	if len(w.loadedModels) == 0 {
		return nil, w.swapping
	}
	out := make([]string, len(w.loadedModels))
	copy(out, w.loadedModels)
	return out, w.swapping
}

func (w *Watcher) setLoaded(models []string, swapping bool) {
	w.loadedMu.Lock()
	w.loadedModels = models
	w.swapping = swapping
	w.loadedMu.Unlock()
}

// modelDetails is the per-digest enriched info populated from /api/show.
// Stored separately from ModelInfo so the cache can be keyed cleanly.
type modelDetails struct {
	Capabilities  []string
	ContextLength int64
}

// Status returns the current diagnostic snapshot. Safe for concurrent
// use; the admin UI poll calls this on every /api/config refresh.
func (w *Watcher) Status() WatcherStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.state
	s.CloudboxURL = w.cfg.CloudboxURL
	s.OllamaURL = w.cfg.OllamaURL
	return s
}

func (w *Watcher) recordPush(modelCount int) {
	w.mu.Lock()
	w.state.LastPushAt = time.Now().UTC()
	w.state.LastModels = modelCount
	w.state.LastError = ""
	w.state.PushCount++
	w.mu.Unlock()
}

func (w *Watcher) recordError(err error) {
	w.mu.Lock()
	w.state.LastError = err.Error()
	w.mu.Unlock()
}

func (w *Watcher) setRunning(v bool) {
	w.mu.Lock()
	w.state.Running = v
	w.mu.Unlock()
}

// New constructs a Watcher with cfg. Validates the required URLs and
// returns an error rather than discovering misconfiguration at Run time.
func New(cfg Config) (*Watcher, error) {
	if strings.TrimSpace(cfg.OllamaURL) == "" {
		return nil, errors.New("ollama watcher: OllamaURL is required")
	}
	if strings.TrimSpace(cfg.CloudboxURL) == "" {
		return nil, errors.New("ollama watcher: CloudboxURL is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = defaultHeartbeatInterval
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	cfg.CloudboxURL = strings.TrimRight(cfg.CloudboxURL, "/")
	cfg.OllamaURL = strings.TrimRight(cfg.OllamaURL, "/")
	kept := cfg.ExtraOllamaURLs[:0]
	seen := map[string]bool{cfg.OllamaURL: true}
	for _, u := range cfg.ExtraOllamaURLs {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		kept = append(kept, u)
	}
	cfg.ExtraOllamaURLs = kept
	return &Watcher{cfg: cfg, detailsCache: map[string]modelDetails{}}, nil
}

// Run blocks until ctx is cancelled. Returns nil on graceful shutdown,
// ErrAuthRevoked if cloudbox 401s (pairing pulled), or a wrapped error
// for unrecoverable misconfiguration. Transient HTTP/network errors do
// NOT propagate — the watcher retries with exponential backoff.
func (w *Watcher) Run(ctx context.Context) error {
	if strings.TrimSpace(w.cfg.AccessToken) == "" {
		w.cfg.Logger.Info("ollama watcher: no access_token; skipping (outpost is unpaired)")
		<-ctx.Done()
		return nil
	}
	w.setRunning(true)
	defer w.setRunning(false)
	w.cfg.Logger.Info("ollama watcher: starting",
		"poll", w.cfg.PollInterval, "heartbeat", w.cfg.HeartbeatInterval,
		"ollama", w.cfg.OllamaURL, "cloudbox", w.cfg.CloudboxURL)

	var (
		lastSnapshot []ModelInfo
		// lastPushedHash is the ContentHash of the model list we last
		// successfully pushed to cloudbox. Empty until the first push
		// completes (cloudbox treats this as "always do Replace" on
		// its side, so the first push behaves like today).
		lastPushedHash string
		// lastPushAt is zero until we've successfully pushed at least
		// once. Heartbeats fire when (now - lastPushAt) >= HeartbeatInterval.
		// Zero-value time forces an initial push on the first tick.
		lastPushAt time.Time
		// backoff: when the local probe or remote push fails, we wait
		// progressively longer to avoid hammering. Reset to zero after
		// a successful push.
		backoff time.Duration
	)

	// Run one cycle immediately so cloudbox learns about this outpost
	// without waiting a full PollInterval — the user just enabled the
	// pool and expects to see their models in the cloud "soon."
	if err := w.tick(ctx, &lastSnapshot, &lastPushedHash, &lastPushAt); err != nil {
		if errors.Is(err, ErrAuthRevoked) {
			w.recordError(err)
			return err
		}
		backoff = w.bumpBackoff(backoff)
		w.recordError(err)
		w.cfg.Logger.Warn("ollama watcher: initial push failed", "err", err, "backoff", backoff)
	}

	for {
		wait := w.cfg.PollInterval
		if backoff > 0 {
			wait = backoff
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			w.cfg.Logger.Info("ollama watcher: shutting down")
			return nil
		case <-t.C:
		}

		err := w.tick(ctx, &lastSnapshot, &lastPushedHash, &lastPushAt)
		switch {
		case err == nil:
			backoff = 0
		case errors.Is(err, ErrAuthRevoked):
			w.recordError(err)
			return err
		case ctx.Err() != nil:
			// A tick cut short by shutdown is not a failure to report.
			w.cfg.Logger.Info("ollama watcher: shutting down")
			return nil
		default:
			backoff = w.bumpBackoff(backoff)
			w.recordError(err)
			w.cfg.Logger.Warn("ollama watcher: tick failed", "err", err, "backoff", backoff)
		}
	}
}

// bumpBackoff returns the next backoff, doubling up to defaultMaxBackoff
// from a defaultMinBackoff floor.
func (w *Watcher) bumpBackoff(cur time.Duration) time.Duration {
	if cur <= 0 {
		return defaultMinBackoff
	}
	return min(cur*2, defaultMaxBackoff)
}

// tick runs one fetch + diff + maybe-push cycle. Updates lastSnapshot,
// lastPushedHash, and lastPushAt by reference so the outer loop can
// drive cadence.
//
// /api/ps is polled best-effort on every tick — its result feeds the
// LoadedSnapshot cache that Service.CapacityHandler reads. A failing
// /api/ps does not abort the tick: the cache is left as-is (stale but
// not empty), and the push still goes through using whatever loaded
// info Service composes from Counter alone.
//
// Push happens when:
//   - the content hash changed since lastPushedHash (real model
//     set/digest/family change), OR
//   - lastPushAt is older than HeartbeatInterval (or zero, on first run).
//
// Note: we no longer push on bare modelsEqual change — Ollama's
// ModifiedAt jitters under stat-syscall noise even when the underlying
// model file hasn't been touched, which used to flood cloudbox with
// false-positive "change detected" pushes. ContentHash projects onto
// the stable fields so timestamp noise is invisible.
func (w *Watcher) tick(ctx context.Context, lastSnapshot *[]ModelInfo, lastPushedHash *string, lastPushAt *time.Time) error {
	w.tickMu.Lock()
	defer w.tickMu.Unlock()
	models, sources, err := w.fetchModelsWithSources(ctx)
	if err != nil {
		return fmt.Errorf("fetch tags: %w", err)
	}
	// Refresh /api/ps cache before the push so the embedded
	// CapacityReport carries the freshest loaded/swapping info. Failure
	// is logged at debug level and ignored — the rest of the tick must
	// still run.
	w.refreshLoaded(ctx)
	// Snapshot the cluster descriptor once per tick (nil on every
	// single-machine outpost) and fold it into the change-detection hash
	// so a membership/backend change re-triggers a push + full cloudbox
	// Replace even when the model list is unchanged.
	var cluster *ClusterCapacity
	if w.cfg.Cluster != nil {
		cluster = w.cfg.Cluster.ClusterSnapshot()
		if src, ok := w.cfg.Cluster.(ClusterModelSource); ok {
			models = mergeModels(models, src.ClusterModels())
		}
	}
	worker := w.workerInfo(ctx)
	currentHash := CombineHash(ContentHash(models), cluster)
	if tag := workerHashTag(worker); tag != "" {
		currentHash = hashString(currentHash + tag)
	}
	changed := currentHash != *lastPushedHash
	heartbeatDue := time.Since(*lastPushAt) >= w.cfg.HeartbeatInterval
	if !changed && !heartbeatDue {
		return nil
	}
	if err := w.probeModels(ctx, models, sources); err != nil {
		return err
	}
	if err := w.push(ctx, models, currentHash, cluster, worker); err != nil {
		return err
	}
	*lastSnapshot = models
	*lastPushedHash = currentHash
	*lastPushAt = time.Now()
	w.recordPush(len(models))
	if changed {
		w.cfg.Logger.Info("ollama watcher: pushed model change", "models", len(models))
	} else {
		w.cfg.Logger.Debug("ollama watcher: pushed heartbeat", "models", len(models))
	}
	return nil
}

// PushNow runs one unconditional fetch + push cycle outside the Run loop
// (the change/heartbeat gate is bypassed). The GPU worker watchdog calls it
// as the host's last word before powering off, so the master sees the
// final state rather than waiting out the freshness clock.
func (w *Watcher) PushNow(ctx context.Context) error {
	if strings.TrimSpace(w.cfg.AccessToken) == "" {
		return errors.New("ollama watcher: no access_token (outpost is unpaired)")
	}
	var (
		snap []ModelInfo
		hash string
		at   time.Time
	)
	return w.tick(ctx, &snap, &hash, &at)
}

// refreshLoaded GETs /api/ps and updates the loadedModels / swapping
// cache. Best-effort: on any failure (404, decode error, transport)
// the cache is left untouched so transient blips don't flip cloudbox
// into thinking models unloaded. Extra daemons (Config.ExtraOllamaURLs)
// are unioned in best-effort — a down extra never wipes the cache.
func (w *Watcher) refreshLoaded(ctx context.Context) {
	names, swapping, ok := w.probeLoaded(ctx, w.cfg.OllamaURL)
	if !ok {
		return
	}
	for _, extra := range w.cfg.ExtraOllamaURLs {
		en, esw, eok := w.probeLoaded(ctx, extra)
		if !eok {
			w.cfg.Logger.Debug("ollama watcher: extra /api/ps probe failed", "url", extra)
			continue
		}
		names = unionStrings(names, en)
		swapping = swapping || esw
	}
	sort.Strings(names)
	w.setLoaded(names, swapping)
}

// probeLoaded GETs one daemon's /api/ps. ok=false on any failure —
// the caller decides whether that is fatal (primary) or skippable.
func (w *Watcher) probeLoaded(ctx context.Context, baseURL string) (names []string, swapping, ok bool) {
	names, swapping, err := ProbeLoaded(ctx, w.cfg.HTTPClient, baseURL)
	if err != nil {
		w.cfg.Logger.Debug("ollama watcher: /api/ps probe failed", "err", err)
		return nil, false, false
	}
	return names, swapping, true
}

// ProbeLoaded GETs one daemon's /api/ps and returns the resident model
// names plus a swap-in-progress hint. Any failure (transport, non-200 —
// a 404 means an old daemon or a non-ollama replacement — or decode) is
// an error; the caller decides whether that is fatal. Nil client →
// http.DefaultClient.
func ProbeLoaded(ctx context.Context, client *http.Client, baseURL string) (names []string, swapping bool, err error) {
	if client == nil {
		client = http.DefaultClient
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/ps", nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("/api/ps: HTTP %d", resp.StatusCode)
	}
	var pr psResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&pr); err != nil {
		return nil, false, fmt.Errorf("/api/ps decode: %w", err)
	}
	now := time.Now()
	names = make([]string, 0, len(pr.Models))
	for _, m := range pr.Models {
		name := m.Name
		if name == "" {
			name = m.Model
		}
		if name == "" {
			continue
		}
		names = append(names, name)
		// "loading" / "pulling" are in-progress signals; either one
		// means the daemon is mid-swap and cloudbox should hold off
		// new dispatches.
		switch strings.ToLower(m.State) {
		case "loading", "pulling":
			swapping = true
		}
		// A non-zero expires_at in the past means this slot has just
		// been (or is being) evicted by LRU/keep-alive. Treat as a
		// swap-in-progress hint too.
		if !m.ExpiresAt.IsZero() && m.ExpiresAt.Before(now) {
			swapping = true
		}
	}
	return names, swapping, nil
}

// unionStrings merges two name lists, deduplicated. Order is normalized
// by the caller (refreshLoaded sorts before caching).
func unionStrings(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, n := range a {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, n := range b {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// fetchModels GETs the local Ollama daemon's /api/tags, unions in every
// extra daemon's inventory (Config.ExtraOllamaURLs — bashy's own engine —
// best-effort: a down extra is skipped, never fatal), then enriches each
// entry with per-digest /api/show metadata (capabilities, context_length)
// from a cache. /api/show goes to the daemon that served the model, so a
// model that lives only on an extra still gets its details. Cache misses
// do one extra request per new digest — a model pull is the only thing
// that adds a row to the inventory, and that's not a hot path. Sorted by
// name so equality comparisons are stable.
func (w *Watcher) fetchModels(ctx context.Context) ([]ModelInfo, error) {
	models, _, err := w.fetchModelsWithSources(ctx)
	return models, err
}

// fetchModelsWithSources returns the inventory plus the daemon that supplied
// each model. The source map lets tick prove the actual serving engine before
// advertising its model to cloudbox.
func (w *Watcher) fetchModelsWithSources(ctx context.Context) ([]ModelInfo, map[string]string, error) {
	models, err := w.fetchTagsFrom(ctx, w.cfg.OllamaURL)
	if err != nil {
		return nil, nil, err
	}
	sources := make(map[string]string, len(models))
	for _, m := range models {
		if _, ok := sources[m.Name]; !ok {
			sources[m.Name] = w.cfg.OllamaURL
		}
	}
	for _, extra := range w.cfg.ExtraOllamaURLs {
		more, err := w.fetchTagsFrom(ctx, extra)
		if err != nil {
			w.cfg.Logger.Debug("ollama watcher: extra /api/tags probe failed", "url", extra, "err", err)
			continue
		}
		for _, m := range more {
			if _, ok := sources[m.Name]; !ok {
				sources[m.Name] = extra
			}
		}
		models = mergeModels(models, more)
	}
	for i := range models {
		d := w.detailsFor(ctx, sources[models[i].Name], models[i])
		models[i].Capabilities = d.Capabilities
		models[i].ContextLength = d.ContextLength
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, sources, nil
}

// probeModels verifies that every locally inventoried model can serve a real
// inference before the watcher publishes it. Cluster-only models are owned by
// their cluster source and therefore have no local source entry here.
func (w *Watcher) probeModels(ctx context.Context, models []ModelInfo, sources map[string]string) error {
	for _, model := range models {
		baseURL := sources[model.Name]
		if baseURL == "" {
			continue
		}
		if err := w.probeInference(ctx, baseURL, model.Name); err != nil {
			return fmt.Errorf("inference probe for %q: %w", model.Name, err)
		}
	}
	return nil
}

func (w *Watcher) probeInference(ctx context.Context, baseURL, model string) error {
	pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	body, err := json.Marshal(map[string]any{
		"model":   model,
		"prompt":  "ready",
		"stream":  false,
		"options": map[string]int{"num_predict": 1},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(pctx, http.MethodPost,
		baseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.cfg.AccessToken)
	resp, err := w.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxInferenceProbeBody+1))
	if err != nil {
		return fmt.Errorf("read inference response: %w", err)
	}
	if len(responseBody) > maxInferenceProbeBody {
		return fmt.Errorf("inference response exceeds %d bytes", maxInferenceProbeBody)
	}
	var result struct {
		Done  bool   `json:"done"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return fmt.Errorf("decode inference response: %w", err)
	}
	if result.Error != "" {
		return fmt.Errorf("inference response error: %s", result.Error)
	}
	if !result.Done {
		return errors.New("inference response missing done=true")
	}
	return nil
}

// fetchTagsFrom GETs one daemon's /api/tags. Any failure is fatal to the
// caller — extras are skipped by fetchModels, the primary aborts the tick.
func (w *Watcher) fetchTagsFrom(ctx context.Context, baseURL string) ([]ModelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := w.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("HTTP %d from %s/api/tags: %s",
			resp.StatusCode, baseURL, strings.TrimSpace(string(body)))
	}
	var tr tagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("decode tags: %w", err)
	}
	return tr.toModels(), nil
}

func mergeModels(base, extra []ModelInfo) []ModelInfo {
	if len(extra) == 0 {
		return base
	}
	merged := make([]ModelInfo, 0, len(base)+len(extra))
	seen := map[string]int{}
	for _, m := range base {
		if strings.TrimSpace(m.Name) == "" {
			continue
		}
		seen[m.Name] = len(merged)
		merged = append(merged, m)
	}
	for _, m := range extra {
		if strings.TrimSpace(m.Name) == "" {
			continue
		}
		if i, ok := seen[m.Name]; ok {
			if merged[i].Size == 0 && m.Size > 0 {
				merged[i].Size = m.Size
			}
			continue
		}
		seen[m.Name] = len(merged)
		merged = append(merged, m)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Name < merged[j].Name })
	return merged
}

// detailsFor returns the cached or freshly-fetched details for one
// model. On any error (probe failure, malformed response) returns the
// zero modelDetails — the model still publishes, just without
// capabilities/context_length. A failed probe is cached as zero so a
// broken model doesn't get re-probed every tick.
func (w *Watcher) detailsFor(ctx context.Context, baseURL string, m ModelInfo) modelDetails {
	key := m.Digest
	if key == "" {
		key = m.Name
	}
	w.detailsMu.Lock()
	if d, ok := w.detailsCache[key]; ok {
		w.detailsMu.Unlock()
		return d
	}
	w.detailsMu.Unlock()
	d := w.fetchDetails(ctx, baseURL, m.Name)
	w.detailsMu.Lock()
	w.detailsCache[key] = d
	w.detailsMu.Unlock()
	return d
}

// fetchDetails POSTs to one daemon's /api/show for one model.
// Best-effort: any non-200 or decode error → zero details, no error
// propagation.
func (w *Watcher) fetchDetails(ctx context.Context, baseURL, name string) modelDetails {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	body := fmt.Sprintf(`{"name":%q}`, name)
	req, err := http.NewRequestWithContext(pctx, http.MethodPost,
		baseURL+"/api/show", strings.NewReader(body))
	if err != nil {
		return modelDetails{}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.cfg.HTTPClient.Do(req)
	if err != nil {
		return modelDetails{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return modelDetails{}
	}
	var sr showResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&sr); err != nil {
		return modelDetails{}
	}
	return modelDetails{
		Capabilities:  sr.Capabilities,
		ContextLength: sr.contextLength(),
	}
}

// push POSTs the RegistryPushPayload to cloudbox. Returns
// ErrAuthRevoked on HTTP 401 so the outer Run loop can stop; any other
// non-2xx returns an error the outer loop will back off and retry on.
// cluster is nil on single-machine outposts and is attached verbatim
// (already folded into contentHash by the caller).
// workerInfo returns the worker block for this push, or nil on a non-worker
// host or when it cannot be built. Fail soft: the push goes out without it.
// Called only from tick, under tickMu.
func (w *Watcher) workerInfo(ctx context.Context) *WorkerInfo {
	if w.cfg.WorkerRequestID == "" {
		return nil
	}
	if w.worker != nil {
		return w.worker
	}
	measure := w.cfg.WorkerGPU
	if measure == nil {
		measure = MeasureNVIDIAGPU
	}
	vram, model, err := measure(ctx)
	var wi *WorkerInfo
	if err == nil {
		wi, err = buildWorkerInfo(w.cfg.WorkerRequestID, vram, model)
	}
	if err != nil {
		if msg := err.Error(); msg != w.workerErr {
			w.workerErr = msg
			w.cfg.Logger.Warn("ollama watcher: omitting worker block from registry push", "err", err)
		}
		return nil
	}
	w.worker, w.workerErr = wi, ""
	w.cfg.Logger.Info("ollama watcher: advertising provisioned GPU worker",
		"vram_bytes", wi.VRAMBytes, "gpu_model", wi.GPUModel)
	return wi
}

func (w *Watcher) push(ctx context.Context, models []ModelInfo, contentHash string, cluster *ClusterCapacity, worker *WorkerInfo) error {
	capReport := CapacityReport{MaxParallel: defaultMaxParallel}
	if w.cfg.Capacity != nil {
		capReport = w.cfg.Capacity.Snapshot()
	}
	payload := RegistryPushPayload{
		AgentName:   w.cfg.AgentName,
		Version:     w.cfg.Version,
		HeartbeatAt: time.Now().UTC(),
		Models:      models,
		Capacity:    capReport,
		ContentHash: contentHash,
		Cluster:     cluster,
		LANEndpoint: w.cfg.LANEndpoint,
		Worker:      worker,
	}
	if w.cfg.WarmStatus != nil {
		payload.WarmBudgetBytes, payload.Busy = w.cfg.WarmStatus()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		w.cfg.CloudboxURL+"/api/v1/llm/registry", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.cfg.AccessToken)
	resp, err := w.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("post registry: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrAuthRevoked
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("registry push: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(b)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// modelsEqual reports whether two model snapshots are byte-identical.
// Both slices are assumed sorted by Name (fetchModels guarantees that).
// reflect.DeepEqual is fine here — slice length is small (tens of
// models on a real machine), so deep comparison is cheaper than
// hashing.
func modelsEqual(a, b []ModelInfo) bool { return reflect.DeepEqual(a, b) }
