// Package ollama owns the outpost-side of the LLM pool: it watches the
// local Ollama daemon's model inventory, publishes the inventory to
// cloudbox so the pool scheduler can route by model presence, and
// tracks in-flight request counts so cloudbox can avoid over-scheduling
// a host with limited GPU capacity.
//
// The shapes in this file are the wire contract with cloudbox. Keep
// them stable; additive changes only.
package ollama

import "time"

// ModelInfo describes one model the local Ollama daemon currently has
// downloaded. Sourced from the daemon's GET /api/tags response; the
// fields kept here are the subset cloudbox needs for routing
// decisions, plus enough provenance (digest, modified_at) that the
// scheduler can distinguish two backends advertising "the same model"
// with different quantizations.
type ModelInfo struct {
	Name          string    `json:"name"`
	Digest        string    `json:"digest,omitempty"`
	Size          int64     `json:"size,omitempty"`
	ModifiedAt    time.Time `json:"modified_at,omitzero"`
	Family        string    `json:"family,omitempty"`
	ParameterSize string    `json:"parameter_size,omitempty"`
	Quantization  string    `json:"quantization,omitempty"`
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
