package ollama

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// ContentHash returns a deterministic sha256 over the stable fields of
// the supplied model list. Used to short-circuit cloudbox-side DB work
// on the /api/v1/llm/registry endpoint: when the next push carries the
// same hash, the receiver doesn't need to DELETE+INSERT every row.
//
// "Stable fields" excludes ModifiedAt — Ollama jitters that timestamp
// under stat-syscall noise on every /api/tags poll even when nothing
// actually changed (the watcher used to use reflect.DeepEqual on the
// whole struct, which is why the change-detection layer fired
// spurious pushes between heartbeats). Excluding the timestamp lets
// identical model state hash to the same value across polls.
//
// The output is hex-encoded so it round-trips through JSON without
// base64-padding noise. Empty input yields a stable empty-marker hash
// (the sha256 of "[]") rather than "" — callers that mean "no hash
// available" should pass through their own empty string rather than
// computing one over an empty slice.
func ContentHash(models []ModelInfo) string {
	// Canonical: sort by Name so push order doesn't change the hash,
	// and project onto a fixed-field shape so a future additive change
	// to ModelInfo (new fields) doesn't silently change every running
	// outpost's hash. Anyone who wants the new field in the hash has
	// to explicitly extend this struct.
	type stable struct {
		Name          string   `json:"name"`
		Digest        string   `json:"digest,omitempty"`
		Size          int64    `json:"size,omitempty"`
		Family        string   `json:"family,omitempty"`
		ParameterSize string   `json:"parameter_size,omitempty"`
		Quantization  string   `json:"quantization,omitempty"`
		Capabilities  []string `json:"capabilities,omitempty"`
		ContextLength int64    `json:"context_length,omitempty"`
	}
	projected := make([]stable, 0, len(models))
	for _, m := range models {
		// Defensive copy + sort capabilities so the order on the wire
		// from Ollama doesn't perturb the hash.
		caps := append([]string(nil), m.Capabilities...)
		sort.Strings(caps)
		projected = append(projected, stable{
			Name:          m.Name,
			Digest:        m.Digest,
			Size:          m.Size,
			Family:        m.Family,
			ParameterSize: m.ParameterSize,
			Quantization:  m.Quantization,
			Capabilities:  caps,
			ContextLength: m.ContextLength,
		})
	}
	sort.Slice(projected, func(i, j int) bool { return projected[i].Name < projected[j].Name })

	buf, err := json.Marshal(projected)
	if err != nil {
		// Marshal of a slice of plain structs with stdlib types can't
		// fail in practice; if it ever does, returning empty falls
		// through to the cloudbox slow path which is correct (just
		// slower).
		return ""
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// clusterHashTag projects a ClusterCapacity onto a short deterministic
// string mixed into the registry push's content hash. Nil (the
// single-machine common case) contributes nothing, so a non-cluster
// outpost's hash is byte-identical to what it was before the cluster
// field existed. When a cluster's membership, aggregate size, or backend
// changes, the tag changes — which flips the combined hash and forces a
// full cloudbox-side Replace even if the model list is unchanged (the
// model-only hash would otherwise let cloudbox fast-path and skip
// persisting the new cluster fields).
func clusterHashTag(c *ClusterCapacity) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("|cluster:%s:%d:%d", c.Backend, c.MemberCount, c.MaxModelBytes)
}

// CombineHash folds a cluster tag into the model-only ContentHash. When
// cluster is nil it returns base unchanged (so single-machine outposts
// keep emitting the exact same content_hash they always have); otherwise
// it re-hashes base+tag so cloudbox observes a change whenever the
// cluster descriptor changes.
func CombineHash(base string, cluster *ClusterCapacity) string {
	tag := clusterHashTag(cluster)
	if tag == "" {
		return base
	}
	sum := sha256.Sum256([]byte(base + tag))
	return hex.EncodeToString(sum[:])
}
