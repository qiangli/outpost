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

// Watcher polls the local Ollama daemon's model inventory and publishes
// changes to cloudbox.
type Watcher struct {
	cfg Config
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
	return &Watcher{cfg: cfg}, nil
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
			return err
		}
		backoff = w.bumpBackoff(backoff)
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
			return err
		default:
			backoff = w.bumpBackoff(backoff)
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
// Push happens when:
//   - the model set changed since lastSnapshot, OR
//   - lastPushAt is older than HeartbeatInterval (or zero, on first run).
func (w *Watcher) tick(ctx context.Context, lastSnapshot *[]ModelInfo, lastPushAt *time.Time) error {
	models, err := w.fetchModels(ctx)
	if err != nil {
		return fmt.Errorf("fetch tags: %w", err)
	}
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
	if changed {
		w.cfg.Logger.Info("ollama watcher: pushed model change", "models", len(models))
	} else {
		w.cfg.Logger.Debug("ollama watcher: pushed heartbeat", "models", len(models))
	}
	return nil
}

// fetchModels GETs the local Ollama daemon's /api/tags and returns the
// normalized ModelInfo list (sorted by name so equality comparisons are
// stable). Returns an empty list (no error) when the daemon answers with
// 404 / 5xx — that's still a heartbeat-worthy data point ("daemon was
// reachable, here is its current state").
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
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, nil
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
