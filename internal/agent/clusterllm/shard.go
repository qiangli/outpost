package clusterllm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
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

// ModelInfo is one model served by an OpenAI-compatible cluster backend.
// It maps onto ollama.ModelInfo at the registry-push edge without coupling
// this package to the ollama wire types.
type ModelInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size,omitempty"`
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
	for _, u := range endpointURLs(endpoint, "/props") {
		body, got := getJSON(ctx, client, u, "")
		if !got {
			continue
		}
		var p llamaProps
		if err := json.Unmarshal(body, &p); err == nil && p.identifies() {
			return p.banner(), true
		}
	}
	// /health is the minimal liveness surface — a status field is enough to
	// confirm llama.cpp even when /props is disabled.
	for _, u := range endpointURLs(endpoint, "/health") {
		body, got := getJSON(ctx, client, u, "")
		if !got {
			continue
		}
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

func listLlamaCPPModels(ctx context.Context, client *http.Client, endpoint string) []ModelInfo {
	models := listOpenAIModels(ctx, client, endpoint)
	if len(models) == 0 {
		return nil
	}
	if len(models) == 1 && models[0].Size == 0 {
		if size := probeLlamaCPPModelSize(ctx, client, endpoint); size > 0 {
			models[0].Size = size
		}
	}
	return models
}

func listOpenAIModels(ctx context.Context, client *http.Client, endpoint string) []ModelInfo {
	for _, u := range openAIModelsURLs(endpoint) {
		body, ok := getJSON(ctx, client, u, "")
		if !ok {
			continue
		}
		models := decodeOpenAIModels(body)
		if len(models) > 0 {
			return models
		}
	}
	return nil
}

func decodeOpenAIModels(body []byte) []ModelInfo {
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	out := make([]ModelInfo, 0, len(resp.Data))
	for _, row := range resp.Data {
		name := firstString(row, "id", "name", "model")
		if name == "" {
			continue
		}
		out = append(out, ModelInfo{Name: name, Size: firstSize(row)})
	}
	return out
}

func probeLlamaCPPModelSize(ctx context.Context, client *http.Client, endpoint string) int64 {
	for _, u := range endpointURLs(endpoint, "/props") {
		body, ok := getJSON(ctx, client, u, "")
		if !ok {
			continue
		}
		var p llamaProps
		if err := json.Unmarshal(body, &p); err != nil {
			continue
		}
		if p.ModelPath == "" {
			continue
		}
		if st, err := os.Stat(p.ModelPath); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
			return st.Size()
		}
	}
	return 0
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}

func firstSize(m map[string]any) int64 {
	for _, k := range []string{"size", "size_bytes", "bytes", "file_size", "disk_size"} {
		if n := jsonNumber(m[k]); n > 0 {
			return n
		}
	}
	for _, k := range []string{"meta", "metadata"} {
		if nested, ok := m[k].(map[string]any); ok {
			if n := firstSize(nested); n > 0 {
				return n
			}
		}
	}
	return 0
}

func jsonNumber(v any) int64 {
	switch x := v.(type) {
	case float64:
		if x > 0 {
			return int64(x)
		}
	case json.Number:
		if n, err := x.Int64(); err == nil && n > 0 {
			return n
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func openAIModelsURLs(endpoint string) []string {
	if strings.HasSuffix(strings.TrimRight(endpoint, "/"), "/v1") {
		return endpointURLs(endpoint, "/models")
	}
	return endpointURLs(endpoint, "/v1/models")
}

func endpointURLs(endpoint, suffix string) []string {
	endpoint = strings.TrimRight(endpoint, "/")
	urls := []string{endpoint + suffix}
	if root := openAIRoot(endpoint); root != "" && root != endpoint {
		urls = append(urls, strings.TrimRight(root, "/")+suffix)
	}
	return urls
}

func openAIRoot(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	if path.Base(strings.TrimRight(u.Path, "/")) != "v1" {
		return endpoint
	}
	u.Path = strings.TrimRight(path.Dir(strings.TrimRight(u.Path, "/")), ".")
	if u.Path == "/" {
		u.Path = ""
	}
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}
