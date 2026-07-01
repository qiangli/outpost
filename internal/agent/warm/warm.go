// Package warm implements the outpost's adaptive, considerate, always-on
// warm-serving plane. It keeps a small, conservative set of LLM models
// resident (zero cold-start) but yields the machine to the user's own
// work the moment they get busy — unloading warm models when the
// system-load profiler reports Busy() and restoring them when the host
// goes idle again.
//
// Two halves:
//
//   - Executor.Apply — the control-endpoint action driven by cloudbox
//     over the matrix tunnel (POST /admin/warm): load a model with a
//     persistent keep-alive, form a shard for a too-big model, or unload
//     a model. Each mode respects the live warm budget and is
//     idempotent.
//   - Executor.RunSupervisor — the considerate yield loop. It tracks the
//     DESIRED warm set (what cloudbox last asked to keep warm, persisted)
//     and, on every tick, unloads that set while the host is busy and
//     restores it (within the current warm budget) once the host is
//     idle. So even mid-request the host protects the user's other work
//     and self-heals when quiet.
//
// The executor talks to the local Ollama daemon and (optionally) the
// shard manager through narrow interfaces so it stays testable and free
// of import cycles.
package warm

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Warm request modes.
const (
	ModeLoad   = "load"
	ModeShard  = "shard"
	ModeUnload = "unload"
)

// Response status values.
const (
	StatusLoaded          = "loaded"
	StatusAlreadyResident = "already_resident"
	StatusShardStarted    = "shard_started"
	StatusAlreadyActive   = "already_active"
	StatusUnloaded        = "unloaded"
	StatusSkippedBusy     = "skipped_busy"
	StatusOverBudget      = "over_budget"
)

// WarmRequest is the POST /admin/warm body. Mode is load|shard|unload.
type WarmRequest struct {
	Model string `json:"model"`
	Mode  string `json:"mode"`
}

// WarmResponse is the endpoint's reply. ActiveModel is the model this
// host is currently serving via a shard (or the model just warmed);
// Busy + WarmBudgetBytes echo the live load verdict so the caller can
// see why a load/shard was skipped.
type WarmResponse struct {
	Status          string `json:"status"`
	ActiveModel     string `json:"active_model,omitempty"`
	Busy            bool   `json:"busy"`
	WarmBudgetBytes int64  `json:"warm_budget_bytes"`
}

// OllamaControl is the subset of local-Ollama operations the executor
// needs. Implemented by ollamaClient (below) against the daemon's HTTP
// API; stubbed in tests.
type OllamaControl interface {
	// EnsureResident makes the model resident with a persistent
	// keep-alive (keep_alive: -1). When pull is true a missing model is
	// pulled first; when false a missing model is a no-op error (the
	// supervisor never blocks a tick on a multi-GB download).
	EnsureResident(ctx context.Context, model string, pull bool) error
	// Release unloads the model (keep_alive: 0). Idempotent.
	Release(ctx context.Context, model string) error
	// ModelSizeBytes returns the model's on-disk size (0 when unknown).
	ModelSizeBytes(ctx context.Context, model string) (uint64, error)
	// LoadedModels returns the names currently resident (/api/ps).
	LoadedModels(ctx context.Context) ([]string, error)
	// OnDisk reports whether the model is already downloaded (/api/tags).
	OnDisk(ctx context.Context, model string) bool
}

// ShardControl is the subset of the shard manager the executor drives.
// Optional — nil disables shard mode.
type ShardControl interface {
	ActiveModel() string
	Orchestrate(ctx context.Context, model string, apiPort int, extra []string) error
	Stop()
}

// LoadGauge is the sysload profiler's warm-serving verdict.
type LoadGauge interface {
	Busy() bool
	WarmBudgetBytes(usableMem uint64) int64
}

// Config wires the executor. Ollama + Gauge are required; Shard is
// optional. UsableMem returns the host's usable memory in bytes.
type Config struct {
	Ollama    OllamaControl
	Shard     ShardControl
	Gauge     LoadGauge
	UsableMem func() uint64
	APIPort   int // shard API port (0 → 11434)

	// Desired seeds the persisted desired warm set (FileConfig.WarmDesired).
	Desired []string
	// PersistDesired persists the updated desired set (threaded through
	// admincore so writes serialize against the config mutex). nil → the
	// desired set is in-memory only (still survives within a daemon run).
	PersistDesired func([]string) error

	Interval time.Duration // supervisor tick (0 → 30s)
	Logger   *slog.Logger
}

// Executor is the warm-serving actor: the control-endpoint handler plus
// the considerate yield/restore supervisor.
type Executor struct {
	cfg     Config
	log     *slog.Logger
	apiPort int

	mu      sync.Mutex
	desired map[string]bool
	yielded bool // supervisor state: are the warm models currently unloaded for a busy host?
}

// New builds an Executor, seeding the desired set from cfg.Desired.
func New(cfg Config) *Executor {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	apiPort := cfg.APIPort
	if apiPort == 0 {
		apiPort = 11434
	}
	e := &Executor{cfg: cfg, log: log, apiPort: apiPort, desired: map[string]bool{}}
	for _, m := range cfg.Desired {
		if m = strings.TrimSpace(m); m != "" {
			e.desired[m] = true
		}
	}
	return e
}

// Desired returns a sorted copy of the current desired warm set.
func (e *Executor) Desired() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.desiredLocked()
}

func (e *Executor) desiredLocked() []string {
	out := make([]string, 0, len(e.desired))
	for m := range e.desired {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// addDesired / removeDesired mutate the set and persist it. Best-effort
// persistence: a failure is logged, never fatal.
func (e *Executor) addDesired(model string) {
	e.mu.Lock()
	if e.desired[model] {
		e.mu.Unlock()
		return
	}
	e.desired[model] = true
	set := e.desiredLocked()
	e.mu.Unlock()
	e.persist(set)
}

func (e *Executor) removeDesired(model string) {
	e.mu.Lock()
	if !e.desired[model] {
		e.mu.Unlock()
		return
	}
	delete(e.desired, model)
	set := e.desiredLocked()
	e.mu.Unlock()
	e.persist(set)
}

func (e *Executor) persist(set []string) {
	if e.cfg.PersistDesired == nil {
		return
	}
	if err := e.cfg.PersistDesired(set); err != nil {
		e.log.Warn("warm: persist desired set failed", "err", err)
	}
}

func (e *Executor) usableMem() uint64 {
	if e.cfg.UsableMem == nil {
		return 0
	}
	return e.cfg.UsableMem()
}

func (e *Executor) budgetBytes() int64 {
	return e.cfg.Gauge.WarmBudgetBytes(e.usableMem())
}

// shardActive returns the shard's active model, or "" when no shard is
// running (or sharding isn't wired).
func (e *Executor) shardActive() string {
	if e.cfg.Shard == nil {
		return ""
	}
	return e.cfg.Shard.ActiveModel()
}

// Apply runs one warm request. Returns an *APIError for client-visible
// failures (bad mode, sharding unavailable) so the HTTP layer can map
// the status code; a plain error is a 500.
func (e *Executor) Apply(ctx context.Context, req WarmRequest) (WarmResponse, error) {
	model := strings.TrimSpace(req.Model)
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	resp := WarmResponse{Busy: e.cfg.Gauge.Busy(), WarmBudgetBytes: e.budgetBytes()}
	if model == "" {
		return resp, badRequest("model is required")
	}
	switch mode {
	case ModeLoad:
		return e.applyLoad(ctx, model, resp)
	case ModeShard:
		return e.applyShard(ctx, model, resp)
	case ModeUnload:
		return e.applyUnload(ctx, model, resp)
	default:
		return resp, badRequest("mode must be one of load / shard / unload")
	}
}

func (e *Executor) applyLoad(ctx context.Context, model string, resp WarmResponse) (WarmResponse, error) {
	// Remember the intent regardless of whether we can act now — the
	// supervisor restores it once the host is idle / has headroom.
	e.addDesired(model)
	if resp.Busy {
		resp.Status = StatusSkippedBusy
		e.log.Info("warm: load skipped — host busy", "model", model)
		return resp, nil
	}
	if size, _ := e.cfg.Ollama.ModelSizeBytes(ctx, model); size > 0 && int64(size) > resp.WarmBudgetBytes {
		resp.Status = StatusOverBudget
		e.log.Info("warm: load skipped — over warm budget", "model", model, "size", size, "budget", resp.WarmBudgetBytes)
		return resp, nil
	}
	loaded, _ := e.cfg.Ollama.LoadedModels(ctx)
	if contains(loaded, model) {
		resp.Status = StatusAlreadyResident
		resp.ActiveModel = model
		return resp, nil
	}
	if err := e.cfg.Ollama.EnsureResident(ctx, model, true); err != nil {
		return resp, internalErr("ensure resident %q: %v", model, err)
	}
	resp.Status = StatusLoaded
	resp.ActiveModel = model
	e.log.Info("warm: loaded (persistent keep-alive)", "model", model)
	return resp, nil
}

func (e *Executor) applyShard(ctx context.Context, model string, resp WarmResponse) (WarmResponse, error) {
	if e.cfg.Shard == nil {
		return resp, unavailable("sharding is not enabled on this host")
	}
	e.addDesired(model)
	if e.cfg.Shard.ActiveModel() == model {
		resp.Status = StatusAlreadyActive
		resp.ActiveModel = model
		return resp, nil
	}
	if resp.Busy {
		resp.Status = StatusSkippedBusy
		e.log.Info("warm: shard skipped — host busy", "model", model)
		return resp, nil
	}
	if resp.WarmBudgetBytes <= 0 {
		resp.Status = StatusOverBudget
		return resp, nil
	}
	// Forming a shard self-provisions + launches prima across the ring,
	// which can take long enough that holding the request open risks a
	// reset. Kick it off in the background (idempotent — a re-trigger for
	// the already-active model no-ops) and report shard_started.
	go func() {
		if err := e.cfg.Shard.Orchestrate(context.Background(), model, e.apiPort, nil); err != nil {
			e.log.Warn("warm: shard orchestrate failed", "model", model, "err", err)
		}
	}()
	resp.Status = StatusShardStarted
	resp.ActiveModel = model
	e.log.Info("warm: shard forming", "model", model)
	return resp, nil
}

func (e *Executor) applyUnload(ctx context.Context, model string, resp WarmResponse) (WarmResponse, error) {
	e.removeDesired(model)
	if err := e.cfg.Ollama.Release(ctx, model); err != nil {
		e.log.Debug("warm: release returned error (continuing)", "model", model, "err", err)
	}
	if e.cfg.Shard != nil && e.cfg.Shard.ActiveModel() == model {
		e.cfg.Shard.Stop()
	}
	resp.Status = StatusUnloaded
	resp.ActiveModel = e.shardActive()
	e.log.Info("warm: unloaded", "model", model)
	return resp, nil
}

// RunSupervisor is the considerate yield loop. It ticks on Interval,
// unloading the desired warm set while the host is busy and restoring it
// (within the current budget) while idle. Always returns nil.
func (e *Executor) RunSupervisor(ctx context.Context) error {
	t := time.NewTicker(e.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			e.reconcile(ctx)
		}
	}
}

// reconcile is one supervisor pass.
func (e *Executor) reconcile(ctx context.Context) {
	desired := e.Desired()
	if len(desired) == 0 {
		return
	}
	if e.cfg.Gauge.Busy() {
		e.yield(ctx, desired)
		return
	}
	e.restore(ctx, desired)
}

// yield unloads the desired warm set (and pauses a desired shard) so the
// host is free for the user's work. Only acts + logs on the idle→busy
// transition to avoid churning the daemon every tick.
func (e *Executor) yield(ctx context.Context, desired []string) {
	e.mu.Lock()
	if e.yielded {
		e.mu.Unlock()
		return
	}
	e.yielded = true
	e.mu.Unlock()

	e.log.Info("warm: host busy — yielding warm set", "models", desired)
	for _, model := range desired {
		if err := e.cfg.Ollama.Release(ctx, model); err != nil {
			e.log.Debug("warm: yield release failed", "model", model, "err", err)
		}
	}
	if e.cfg.Shard != nil {
		if active := e.cfg.Shard.ActiveModel(); active != "" && contains(desired, active) {
			e.log.Info("warm: pausing shard for busy host", "model", active)
			e.cfg.Shard.Stop()
		}
	}
}

// restore reloads any desired model that's on disk, not currently
// resident, and fits the live warm budget. Runs every idle tick so a
// busy→idle transition self-heals and an over-budget model is retried
// once there's headroom. Never pulls (a multi-GB download must not block
// a tick — that only happens on an explicit load request).
func (e *Executor) restore(ctx context.Context, desired []string) {
	e.mu.Lock()
	wasYielded := e.yielded
	e.yielded = false
	e.mu.Unlock()
	if wasYielded {
		e.log.Info("warm: host idle — restoring warm set", "models", desired)
	}

	loaded, _ := e.cfg.Ollama.LoadedModels(ctx)
	budget := e.budgetBytes()
	for _, model := range desired {
		if contains(loaded, model) {
			continue
		}
		// A too-big shard model (size > budget) is left to the shard
		// auto-trigger; loading it whole would blow past the budget.
		if size, _ := e.cfg.Ollama.ModelSizeBytes(ctx, model); size > 0 && int64(size) > budget {
			continue
		}
		if !e.cfg.Ollama.OnDisk(ctx, model) {
			continue
		}
		if err := e.cfg.Ollama.EnsureResident(ctx, model, false); err != nil {
			e.log.Debug("warm: restore failed", "model", model, "err", err)
			continue
		}
		e.log.Info("warm: restored", "model", model)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// --- typed errors (mirrors admincore's shape without importing it) ---

// APIError carries an HTTP-style status alongside the message so the
// route layer can map it to a gin status code.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

func badRequest(format string, args ...any) error {
	return &APIError{Status: http.StatusBadRequest, Msg: fmt.Sprintf(format, args...)}
}
func unavailable(format string, args ...any) error {
	return &APIError{Status: http.StatusServiceUnavailable, Msg: fmt.Sprintf(format, args...)}
}
func internalErr(format string, args ...any) error {
	return &APIError{Status: http.StatusInternalServerError, Msg: fmt.Sprintf(format, args...)}
}
