package clusterllm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// BackendLlamaCPP is the second driver: a llama.cpp RPC shard — one leader
// `llama-server --rpc <worker IPs>` pipelining tensor work to N
// `rpc-server` workers (the topology `outpost cluster shard-init`
// scaffolds). It is the vk-ollama "model bigger than any one box's VRAM"
// path: several LAN boxes cooperatively serve one model.
//
// Unlike GPUStack the shard exposes no Bearer-gated management API to sum
// worker VRAM from, so MaxModelBytes stays unknown (0, filter inert) when a
// shard leader is detected. What the seam *does* carry is the serving
// endpoint + the llamacpp backend tag, which is exactly what's needed for
// cloudbox's tier-0 router to discover the home and route to it; the size
// filter (clusterCanHold) is proprietary + separate and treats an unknown
// MaxModelBytes as unconstrained, so the cluster is never hidden.
const BackendLlamaCPP = "llamacpp"

// ShardEndpoint returns the loopback serving URL a llama.cpp shard leader
// listens on for the given OpenAI/Ollama API port. This is the value to
// wire into cluster_llm_endpoint on the node running the leader so the
// outpost there detects the shard and advertises it through the existing
// seam. `outpost cluster shard-init` prints this as a formation-time
// advisory.
func ShardEndpoint(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// llamaProps is a tolerant decode of llama-server's /props. Only the fields
// that prove "this is a llama.cpp leader" and supply a cosmetic version
// banner are read; everything else is ignored. Field names vary across
// llama.cpp releases, so several version-ish keys are tried.
type llamaProps struct {
	BuildInfo string `json:"build_info"`
	Version   string `json:"version"`
	ModelPath string `json:"model_path"`
	// total_slots is present on every modern llama-server; its presence is
	// a positive identity signal even when no version banner is exposed.
	TotalSlots *int `json:"total_slots"`
}

// llamaHealth is llama-server's /health body: {"status":"ok"} (200) or a
// loading/error status. Any decodable status field proves a llama.cpp
// server is listening.
type llamaHealth struct {
	Status string `json:"status"`
}

// probeLlamaCPP positively identifies a llama.cpp shard leader at endpoint
// and returns a best-effort version banner. ok=false means "not a llama.cpp
// leader" (or unreachable) — the caller then leaves the backend tag as-is,
// so this never misfires on a GPUStack server (which 404s these paths).
//
// Identity requires a *positive* signal — a /health with a status field, or
// a /props with total_slots — never just a 2xx, because GPUStack's OpenAI
// alias also 2xxes. Version is cosmetic; empty on any failure.
func probeLlamaCPP(ctx context.Context, client *http.Client, endpoint string) (version string, ok bool) {
	// /props is the richest identity + version surface; try it first.
	if body, got := getJSON(ctx, client, endpoint+"/props", ""); got {
		var p llamaProps
		if err := json.Unmarshal(body, &p); err == nil && p.identifies() {
			return p.banner(), true
		}
	}
	// /health is the minimal liveness surface — a status field is enough to
	// confirm llama.cpp even when /props is disabled.
	if body, got := getJSON(ctx, client, endpoint+"/health", ""); got {
		var h llamaHealth
		if err := json.Unmarshal(body, &h); err == nil && strings.TrimSpace(h.Status) != "" {
			return "", true
		}
	}
	return "", false
}

func (p llamaProps) identifies() bool {
	return p.TotalSlots != nil ||
		strings.TrimSpace(p.BuildInfo) != "" ||
		strings.TrimSpace(p.ModelPath) != ""
}

func (p llamaProps) banner() string {
	if v := strings.TrimSpace(p.BuildInfo); v != "" {
		return v
	}
	return strings.TrimSpace(p.Version)
}
