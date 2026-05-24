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
// GET /app/ollama/_pool/capacity. MaxParallel is the upper bound the
// daemon will serve concurrently (derived from OLLAMA_NUM_PARALLEL,
// default 4). InFlight is the count of currently-streaming chat /
// generate / embeddings requests as observed by the outpost proxy.
type CapacityReport struct {
	MaxParallel int `json:"max_parallel"`
	InFlight    int `json:"in_flight"`
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
