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
)

// CapacitySource is what the watcher calls to populate the capacity
// section of each push. Decoupled from *Counter so tests can plug a
// stub and so the watcher does not own the request-counting handler.
type CapacitySource interface {
	Snapshot() CapacityReport
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
	if err := w.tick(ctx, &lastSnapshot, &lastPushAt); err != nil {
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

		err := w.tick(ctx, &lastSnapshot, &lastPushAt)
		switch {
		case err == nil:
			backoff = 0
		case errors.Is(err, ErrAuthRevoked):
			w.recordError(err)
			return err
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

// tick runs one fetch + diff + maybe-push cycle. Updates lastSnapshot
// and lastPushAt by reference so the outer loop can drive cadence.
//
// /api/ps is polled best-effort on every tick — its result feeds the
// LoadedSnapshot cache that Service.CapacityHandler reads. A failing
// /api/ps does not abort the tick: the cache is left as-is (stale but
// not empty), and the push still goes through using whatever loaded
// info Service composes from Counter alone.
//
// Push happens when:
//   - the model set changed since lastSnapshot, OR
//   - lastPushAt is older than HeartbeatInterval (or zero, on first run).
func (w *Watcher) tick(ctx context.Context, lastSnapshot *[]ModelInfo, lastPushAt *time.Time) error {
	models, err := w.fetchModels(ctx)
	if err != nil {
		return fmt.Errorf("fetch tags: %w", err)
	}
	// Refresh /api/ps cache before the push so the embedded
	// CapacityReport carries the freshest loaded/swapping info. Failure
	// is logged at debug level and ignored — the rest of the tick must
	// still run.
	w.refreshLoaded(ctx)
	changed := !modelsEqual(*lastSnapshot, models)
	heartbeatDue := time.Since(*lastPushAt) >= w.cfg.HeartbeatInterval
	if !changed && !heartbeatDue {
		return nil
	}
	if err := w.push(ctx, models); err != nil {
		return err
	}
	*lastSnapshot = models
	*lastPushAt = time.Now()
	w.recordPush(len(models))
	if changed {
		w.cfg.Logger.Info("ollama watcher: pushed model change", "models", len(models))
	} else {
		w.cfg.Logger.Debug("ollama watcher: pushed heartbeat", "models", len(models))
	}
	return nil
}

// refreshLoaded GETs /api/ps and updates the loadedModels / swapping
// cache. Best-effort: on any failure (404, decode error, transport)
// the cache is left untouched so transient blips don't flip cloudbox
// into thinking models unloaded.
func (w *Watcher) refreshLoaded(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, w.cfg.OllamaURL+"/api/ps", nil)
	if err != nil {
		return
	}
	resp, err := w.cfg.HTTPClient.Do(req)
	if err != nil {
		w.cfg.Logger.Debug("ollama watcher: /api/ps probe failed", "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 404 on /api/ps means the daemon is older than 0.1.x or the
		// probe is being served by a non-ollama replacement; don't
		// alarm, just leave the cache stale.
		return
	}
	var pr psResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&pr); err != nil {
		w.cfg.Logger.Debug("ollama watcher: /api/ps decode failed", "err", err)
		return
	}
	now := time.Now()
	names := make([]string, 0, len(pr.Models))
	swapping := false
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
	sort.Strings(names)
	w.setLoaded(names, swapping)
}

// fetchModels GETs the local Ollama daemon's /api/tags, then enriches
// each entry with per-digest /api/show metadata (capabilities,
// context_length) from a cache. Cache misses do one extra request per
// new digest — a model pull is the only thing that adds a row to the
// inventory, and that's not a hot path. Sorted by name so equality
// comparisons are stable.
func (w *Watcher) fetchModels(ctx context.Context) ([]ModelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.cfg.OllamaURL+"/api/tags", nil)
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
			resp.StatusCode, w.cfg.OllamaURL, strings.TrimSpace(string(body)))
	}
	var tr tagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("decode tags: %w", err)
	}
	models := tr.toModels()
	for i := range models {
		d := w.detailsFor(ctx, models[i])
		models[i].Capabilities = d.Capabilities
		models[i].ContextLength = d.ContextLength
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, nil
}

// detailsFor returns the cached or freshly-fetched details for one
// model. On any error (probe failure, malformed response) returns the
// zero modelDetails — the model still publishes, just without
// capabilities/context_length. A failed probe is cached as zero so a
// broken model doesn't get re-probed every tick.
func (w *Watcher) detailsFor(ctx context.Context, m ModelInfo) modelDetails {
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
	d := w.fetchDetails(ctx, m.Name)
	w.detailsMu.Lock()
	w.detailsCache[key] = d
	w.detailsMu.Unlock()
	return d
}

// fetchDetails POSTs to /api/show for one model. Best-effort: any
// non-200 or decode error → zero details, no error propagation.
func (w *Watcher) fetchDetails(ctx context.Context, name string) modelDetails {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	body := fmt.Sprintf(`{"name":%q}`, name)
	req, err := http.NewRequestWithContext(pctx, http.MethodPost,
		w.cfg.OllamaURL+"/api/show", strings.NewReader(body))
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
func (w *Watcher) push(ctx context.Context, models []ModelInfo) error {
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
