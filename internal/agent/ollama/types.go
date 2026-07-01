// Package ollama owns the outpost-side of the LLM pool: it watches the
// local Ollama daemon's model inventory, publishes the inventory to
// cloudbox so the pool scheduler can route by model presence, and
// tracks in-flight request counts so cloudbox can avoid over-scheduling
// a host with limited GPU capacity.
//
// The shapes in this file are the wire contract with cloudbox. Keep
// them stable; additive changes only.
package ollama

import (
	"strconv"
	"strings"
	"time"
)

// ModelInfo describes one model the local Ollama daemon currently has
// downloaded. Sourced from the daemon's GET /api/tags response; the
// fields kept here are the subset cloudbox needs for routing
// decisions, plus enough provenance (digest, modified_at) that the
// scheduler can distinguish two backends advertising "the same model"
// with different quantizations.
//
// Capabilities and ContextLength are enriched from GET /api/show per
// digest (the watcher caches per-digest to avoid re-probing unchanged
// models). Capabilities is the verbatim list Ollama 0.5+ emits —
// strings like "completion", "tools", "vision", "embedding".
// ContextLength is the max sequence length the model accepts;
// surfaced because client libraries often need it to right-size
// prompts.
type ModelInfo struct {
	Name          string    `json:"name"`
	Digest        string    `json:"digest,omitempty"`
	Size          int64     `json:"size,omitempty"`
	ModifiedAt    time.Time `json:"modified_at,omitzero"`
	Family        string    `json:"family,omitempty"`
	ParameterSize string    `json:"parameter_size,omitempty"`
	Quantization  string    `json:"quantization,omitempty"`
	Capabilities  []string  `json:"capabilities,omitempty"`
	ContextLength int64     `json:"context_length,omitempty"`
}

// CapacityReport is the live load+limit snapshot returned by
// GET /app/ollama/_pool/capacity and embedded in every registry push.
//
// MaxParallel is the upper bound the daemon will serve concurrently
// (derived from OLLAMA_NUM_PARALLEL, default 4). InFlight is the count
// of currently-streaming chat / generate / embeddings requests as
// observed by the outpost proxy. Both fields are present in v1 and v2.
//
// The remaining fields are v2-only — additive, so a v1 cloudbox parser
// keeps decoding cleanly and a v1 outpost (omitting them) marshals to
// the v1 shape thanks to omitempty / omitzero. The schema-discipline
// rule is: never remove or repurpose an existing JSON key; new fields
// only.
//
//   - Version is the schema marker. Absent / zero ⇒ v1. v2 outposts
//     set it to 2 so a router can branch explicitly when needed.
//   - Queued approximates the daemon's pending-but-not-yet-running
//     queue depth as max(0, in_flight - max_parallel). Ollama doesn't
//     expose the real OLLAMA_MAX_QUEUE depth, so this is the best
//     signal until they do; cloudbox's scheduler uses it as a "are we
//     already piling work on this host" hint, not as ground truth.
//   - LoadedModels is the list of model names currently resident in
//     VRAM/RAM as reported by /api/ps. Used by cloudbox to prefer
//     hosts where a candidate model is already warm (load-thrash
//     avoidance) and as a second source for the loaded-cache that
//     llm_loaded.go maintains via its own /api/ps probes.
//   - Swapping is true when /api/ps shows a model in a loading state
//     or its expires_at is in the past. Cloudbox treats this host as
//     having zero free slots for one capacity-cache window (~3 s) so
//     the next routing decision doesn't pile a fresh request onto a
//     daemon that's mid-swap.
//   - NumLoadedMax mirrors OLLAMA_MAX_LOADED_MODELS so cloudbox can
//     reason about per-host model packing. Zero means "use ollama's
//     default" — cloudbox should not treat 0 as "no models can load."
//   - KeepAliveS mirrors OLLAMA_KEEP_ALIVE in seconds. -1 means "pin
//     forever" (matches ollama's sentinel). Zero means "use ollama's
//     default" (5 m at time of writing).
//   - InstanceID is reserved for the eventual multi-Ollama-per-host
//     payload (see ollama-ha-plan.md Phase 0 Track A#2). Today every
//     outpost reports one capacity; this field is empty. Holding the
//     name now keeps a future split from competing for it.
type CapacityReport struct {
	Version      int      `json:"version,omitempty"`
	MaxParallel  int      `json:"max_parallel"`
	InFlight     int      `json:"in_flight"`
	Queued       int      `json:"queued,omitempty"`
	LoadedModels []string `json:"loaded_models,omitempty"`
	Swapping     bool     `json:"swapping,omitempty"`
	NumLoadedMax int      `json:"num_loaded_max,omitempty"`
	KeepAliveS   int      `json:"keep_alive_s,omitempty"`
	// MaxQueue mirrors OLLAMA_MAX_QUEUE so cloudbox can sanity-check
	// that the daemon's invisible queue isn't taking up the slack the
	// cluster-side scheduler is supposed to own (recommended 64; the
	// daemon's default is 512). Zero ⇒ use the daemon default.
	MaxQueue   int    `json:"max_queue,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
}

// psResponse is the subset of GET /api/ps the watcher decodes. Ollama
// returns one entry per loaded model with name, digest, size, and an
// expires_at timestamp. State strings vary across versions — we accept
// "loading" / "pulling" as in-progress signals — and a zero
// expires_at or one in the past flags a model that's about to (or
// just did) unload.
type psResponse struct {
	Models []psModel `json:"models"`
}

type psModel struct {
	Name      string    `json:"name"`
	Model     string    `json:"model,omitempty"`
	Digest    string    `json:"digest,omitempty"`
	Size      int64     `json:"size,omitempty"`
	SizeVRAM  int64     `json:"size_vram,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	State     string    `json:"state,omitempty"`
}

// ClusterCapacity describes an intra-home distributed-inference cluster
// fronting this outpost's Ollama surface — a backend (GPUStack first;
// any runtime that later publishes the same OpenAI /v1 shape) that
// tensor/pipeline-splits a single model across several member machines.
// Present only when the outpost detects such a backend; nil/omitted
// means "single machine," which is every outpost today. Additive to the
// registry contract — a v1 cloudbox ignores the field, a non-cluster
// outpost marshals nil.
//
//   - MaxModelBytes is the largest model (by byte size) the cluster can
//     hold across all member nodes' aggregate VRAM/RAM. Cloudbox's
//     tier-0 router filter drops this host for a requested model larger
//     than this. Zero means "unknown" — the filter stays inert (treats
//     the host as unconstrained) so an older/partial backend never hides
//     models that would otherwise route here.
//   - MemberCount is the number of worker nodes in the cluster (1 when a
//     backend is up but reports a single worker, or when the worker-list
//     probe is unavailable on an older backend).
//   - Backend names the runtime ("gpustack", "distributed-llama", …) so
//     a future swap doesn't need a cloudbox-side re-migration.
type ClusterCapacity struct {
	MaxModelBytes uint64 `json:"max_model_bytes,omitempty"`
	MemberCount   int    `json:"member_count,omitempty"`
	Backend       string `json:"backend,omitempty"`
}

// RegistryPushPayload is what the watcher POSTs to cloudbox's
// /api/v1/llm/registry endpoint. The agent identifies itself via the
// bearer access_token; AgentName is the convenience copy so cloudbox
// can sanity-check the key matches the token.
//
// HeartbeatAt is the moment the agent built the snapshot (RFC3339).
// Cloudbox uses last-seen time to mark an agent offline when no
// heartbeat arrives within its timeout window. Version is the outpost
// short-commit so cloudbox can surface "this agent is stale" without
// a separate probe.
type RegistryPushPayload struct {
	AgentName   string         `json:"agent_name"`
	Version     string         `json:"version,omitempty"`
	HeartbeatAt time.Time      `json:"heartbeat_at"`
	Models      []ModelInfo    `json:"models"`
	Capacity    CapacityReport `json:"capacity"`

	// Cluster is the optional intra-home distributed-inference descriptor
	// (see ClusterCapacity). Nil on every single-machine outpost — the
	// common case — so the field is omitted from the wire entirely. Folded
	// into ContentHash via clusterHashTag so a membership/backend change
	// re-triggers a full cloudbox Replace even when the model list is
	// unchanged.
	Cluster *ClusterCapacity `json:"cluster,omitempty"`

	// LANEndpoint is the direct LAN inference URL (e.g.
	// "http://192.0.2.10:11435/v1") this host serves when the operator has
	// opted into the same-LAN direct-inference listener (lan_inference).
	// Cloudbox may hand it to a caller it detects on the same LAN so the
	// caller reaches this outpost's LLM directly — bypassing the cloudbox
	// relay for lower latency — while still falling back to the Bearer-authed
	// cloudbox /v1 gateway for remote callers. Empty (omitted) on every host
	// that hasn't enabled lan_inference — the common case.
	LANEndpoint string `json:"lan_endpoint,omitempty"`

	// ContentHash is sha256 over the stable fields of the model list
	// (name, digest, size, family, parameter_size, quantization,
	// capabilities, context_length — NOT modified_at, which Ollama
	// jitters under filesystem-stat noise even when nothing changed).
	// When cloudbox receives a payload whose ContentHash matches the
	// one it cached on the previous push, the receiver fast-paths to
	// a single UPDATE on last_seen_at instead of running the
	// transactional DELETE+INSERT that Replace performs.
	//
	// Empty is the legacy/sentinel value — cloudbox treats it as
	// "always run Replace". Pre-content-hash outposts marshal to
	// empty via omitempty and keep working unchanged.
	ContentHash string `json:"content_hash,omitempty"`
}

// tagsResponse is the on-the-wire shape of GET /api/tags as Ollama
// emits it. Decoded once and converted to []ModelInfo for the push
// payload (the conversion drops fields the scheduler doesn't read so
// the bytes-on-the-wire to cloudbox stays small).
type tagsResponse struct {
	Models []struct {
		Name       string    `json:"name"`
		Model      string    `json:"model"`
		ModifiedAt time.Time `json:"modified_at"`
		Size       int64     `json:"size"`
		Digest     string    `json:"digest"`
		Details    struct {
			Family            string `json:"family"`
			ParameterSize     string `json:"parameter_size"`
			QuantizationLevel string `json:"quantization_level"`
		} `json:"details"`
	} `json:"models"`
}

// showResponse is the subset of GET /api/show we care about. Ollama
// returns much more (modelfile, parameters, template, …) — we ignore
// it to keep memory + cache size predictable.
type showResponse struct {
	Capabilities []string       `json:"capabilities"`
	ModelInfo    map[string]any `json:"model_info"`
}

// contextLength digs the architecture-specific context_length out of
// the model_info map. Ollama keys it by architecture
// (`llama.context_length`, `qwen2.context_length`, etc.), so we scan
// for the first key ending in `.context_length` rather than hardcode
// every architecture. Falls back to a top-level `context_length` if
// present.
func (sr showResponse) contextLength() int64 {
	if v, ok := sr.ModelInfo["context_length"]; ok {
		return toInt64(v)
	}
	for k, v := range sr.ModelInfo {
		if strings.HasSuffix(k, ".context_length") {
			return toInt64(v)
		}
	}
	return 0
}

// toInt64 coerces JSON numeric values (which decode to float64 by
// default) and string forms to int64. Returns 0 on any other shape.
func toInt64(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case int64:
		return x
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	}
	return 0
}

func (tr tagsResponse) toModels() []ModelInfo {
	out := make([]ModelInfo, 0, len(tr.Models))
	for _, m := range tr.Models {
		name := m.Name
		if name == "" {
			name = m.Model
		}
		if name == "" {
			continue
		}
		out = append(out, ModelInfo{
			Name:          name,
			Digest:        m.Digest,
			Size:          m.Size,
			ModifiedAt:    m.ModifiedAt,
			Family:        m.Details.Family,
			ParameterSize: m.Details.ParameterSize,
			Quantization:  m.Details.QuantizationLevel,
		})
	}
	return out
}
