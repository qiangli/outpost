// Package clusterllm is outpost's passive integrator for an intra-home
// distributed-inference backend — a runtime that tensor/pipeline-splits a
// single model across several member machines so a home can serve a model
// too large for any one box. It mirrors the builtin_apps DetectOllama /
// DetectPodman shape exactly: HTTP-probe only, never spawn, never manage a
// lifecycle. Whatever started the backend (the operator's `podman run`
// against the ycode-published socket, a future vkpodman-scheduled Pod,
// anything else) is irrelevant here.
//
// The first and only driver today is GPUStack (Apache-2.0,
// OpenAI-compatible, heterogeneous NVIDIA/AMD/Apple/Ascend). The package is
// written backend-agnostic on purpose: a distributed-llama or native-ycode
// backend that publishes the same reachability + worker shape drops in by
// adding a driver and a Backend string, with no change to the outpost↔
// cloudbox wire seam (the ollama registry push) or to cloudbox's tier-0
// router.
//
// Safety contract: every failure mode (endpoint down, wrong path, missing
// API key, schema drift) degrades to an inert result — State NotReachable,
// or Running with MaxModelBytes 0 — so cloudbox's size filter stays off and
// never hides a model that would otherwise route to this host. The backend
// only ever *adds* reach; it can never subtract it by misbehaving.
package clusterllm

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Backend names. The default driver is GPUStack; the string is carried
// through to cloudbox on the registry push so a future swap needs no
// cloudbox-side re-migration.
const (
	BackendGPUStack = "gpustack"
)

// State is the coarse health of the configured backend.
type State string

const (
	// StateUnconfigured: no endpoint set — detection is off entirely.
	StateUnconfigured State = "unconfigured"
	// StateRunning: the endpoint answered an HTTP probe (any status,
	// including 401 — auth-required still proves a daemon is listening).
	StateRunning State = "running"
	// StateNotReachable: the endpoint is configured but nothing answered.
	StateNotReachable State = "not_reachable"
)

// Config is the operator-supplied wiring, sourced from FileConfig
// (cluster_llm_endpoint / cluster_llm_api_key). Empty Endpoint disables
// detection. APIKey is optional: without it the backend is still detected
// as Running, but the worker/VRAM aggregation that powers MaxModelBytes
// needs the key (GPUStack's management API is Bearer-gated), so the
// cloudbox size filter stays inert until a key is provided.
type Config struct {
	Endpoint string
	APIKey   string
	Backend  string // optional override; "" ⇒ BackendGPUStack
}

func (c Config) backend() string {
	if b := strings.TrimSpace(c.Backend); b != "" {
		return b
	}
	return BackendGPUStack
}

// Info is the detection result. AggregateVRAMBytes is the summed total
// memory across all member GPU devices (0 when unknown); the watcher maps
// it onto the registry push's ClusterCapacity.MaxModelBytes, and the admin
// UI renders the whole struct.
type Info struct {
	Backend            string `json:"backend,omitempty"`
	State              State  `json:"state"`
	Endpoint           string `json:"endpoint,omitempty"`
	Version            string `json:"version,omitempty"`
	MemberCount        int    `json:"member_count,omitempty"`
	AggregateVRAMBytes uint64 `json:"aggregate_vram_bytes,omitempty"`
}

// DefaultEndpoint is the conventional loopback port operators publish a
// GPUStack container on. Only a default for documentation/UI hints — the
// operator picks the real host port when launching the container.
const DefaultEndpoint = "http://127.0.0.1:18080"

const (
	probeTimeout = 2 * time.Second
	// defaultTTL keeps back-to-back callers (the registry-push tick and
	// the /apps sysinfo poll) from each firing a fresh probe. Short enough
	// that a worker joining/leaving the cluster shows up within a cadence.
	defaultTTL = 30 * time.Second
)

// Detect runs a one-shot probe of the configured backend. Used by tests
// and by the cached Detector below. ctx bounds the network calls; callers
// without a natural ctx (the watcher's ClusterSnapshot) pass a background
// ctx and rely on the internal per-request timeouts.
func Detect(ctx context.Context, cfg Config, client *http.Client) Info {
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	if endpoint == "" {
		return Info{State: StateUnconfigured}
	}
	if client == nil {
		client = http.DefaultClient
	}
	info := Info{
		Backend:  cfg.backend(),
		Endpoint: endpoint,
		State:    StateNotReachable,
	}
	// Reachability: the OpenAI alias is the most stable public surface.
	// A 401 (auth required) still proves a daemon is listening, so any
	// HTTP status flips us to Running; only a dial/transport failure
	// leaves us NotReachable.
	if !probeReachable(ctx, client, endpoint) {
		return info
	}
	info.State = StateRunning
	info.MemberCount = 1 // a reachable backend is at least one node

	// Aggregate worker/VRAM data — best-effort, key-gated. Any failure
	// leaves MemberCount=1, AggregateVRAMBytes=0 (filter stays inert).
	if agg, ok := aggregateGPUStack(ctx, client, endpoint, cfg.APIKey); ok {
		if agg.members > 0 {
			info.MemberCount = agg.members
		}
		info.AggregateVRAMBytes = agg.vramBytes
		info.Version = agg.version
	}
	return info
}

// probeReachable returns true when the endpoint answers any HTTP status.
// Mirrors builtin_apps.probeHTTP semantics (a listening daemon is the
// signal; only dial/transport errors fail). Probes the GPUStack
// OpenAI-compatible alias, which exists on every supported version.
func probeReachable(ctx context.Context, client *http.Client, endpoint string) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1-openai/models", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode > 0
}

// Detector caches Detect for a short TTL so the registry-push tick and the
// /apps poll don't each probe the backend on every cycle. Mirrors
// agent.BuiltinDetector. A zero/empty-endpoint Config makes Info() a cheap
// constant (StateUnconfigured) with no network calls.
type Detector struct {
	cfg    Config
	ttl    time.Duration
	client *http.Client
	now    func() time.Time

	mu     sync.Mutex
	cached Info
	at     time.Time
	primed bool
}

// NewDetector returns a detector over cfg with the given probe-result TTL
// (0 ⇒ defaultTTL). client may be nil (http.DefaultClient).
func NewDetector(cfg Config, ttl time.Duration, client *http.Client) *Detector {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &Detector{cfg: cfg, ttl: ttl, client: client, now: time.Now}
}

// Info returns the cached or freshly-probed detection result. Safe for
// concurrent use. An unconfigured detector never touches the network.
func (d *Detector) Info(ctx context.Context) Info {
	d.mu.Lock()
	defer d.mu.Unlock()
	if strings.TrimSpace(d.cfg.Endpoint) == "" {
		return Info{State: StateUnconfigured}
	}
	if d.primed && d.now().Sub(d.at) < d.ttl {
		return d.cached
	}
	d.cached = Detect(ctx, d.cfg, d.client)
	d.at = d.now()
	d.primed = true
	return d.cached
}
