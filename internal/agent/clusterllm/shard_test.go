package clusterllm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// llamaShardStub stands in for a llama.cpp shard leader (llama-server
// --rpc). It answers /health and /props the way a real leader does and
// 404s GPUStack's management + OpenAI-alias paths, so the auto-detect logic
// must distinguish it from a GPUStack server purely by which probes succeed.
func llamaShardStub(t *testing.T, withProps bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	if withProps {
		mux.HandleFunc("/props", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"total_slots":4,"build_info":"b4567","model_path":"/models/llama-70b.gguf"}`))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestShardEndpoint(t *testing.T) {
	if got := ShardEndpoint(8080); got != "http://127.0.0.1:8080" {
		t.Fatalf("ShardEndpoint(8080) = %q", got)
	}
}

func TestDetect_LlamaCPP_Explicit(t *testing.T) {
	srv := llamaShardStub(t, true)
	got := Detect(context.Background(),
		Config{Endpoint: srv.URL, Backend: BackendLlamaCPP}, srv.Client())
	if got.State != StateRunning {
		t.Fatalf("want Running, got %q", got.State)
	}
	if got.Backend != BackendLlamaCPP {
		t.Fatalf("want backend %q, got %q", BackendLlamaCPP, got.Backend)
	}
	if got.MemberCount != 1 {
		t.Fatalf("want MemberCount 1, got %d", got.MemberCount)
	}
	// No management API to sum from ⇒ filter stays inert.
	if got.AggregateVRAMBytes != 0 {
		t.Fatalf("want 0 VRAM (no shard mgmt API), got %d", got.AggregateVRAMBytes)
	}
	if got.Version != "b4567" {
		t.Fatalf("want version banner from /props, got %q", got.Version)
	}
	if got.Endpoint != srv.URL {
		t.Fatalf("want endpoint %q advertised, got %q", srv.URL, got.Endpoint)
	}
}

func TestDetect_LlamaCPP_AutoDetect(t *testing.T) {
	// Unset backend + no GPUStack management API ⇒ the auto-detect fallback
	// must positively identify the llama.cpp leader and retag it, rather
	// than mislabeling the home as gpustack.
	srv := llamaShardStub(t, true)
	got := Detect(context.Background(), Config{Endpoint: srv.URL}, srv.Client())
	if got.State != StateRunning {
		t.Fatalf("want Running, got %q", got.State)
	}
	if got.Backend != BackendLlamaCPP {
		t.Fatalf("auto-detect: want backend %q, got %q", BackendLlamaCPP, got.Backend)
	}
}

func TestDetect_LlamaCPP_HealthOnly(t *testing.T) {
	// /props disabled: /health alone is a sufficient identity signal.
	srv := llamaShardStub(t, false)
	got := Detect(context.Background(),
		Config{Endpoint: srv.URL, Backend: BackendLlamaCPP}, srv.Client())
	if got.Backend != BackendLlamaCPP {
		t.Fatalf("want backend %q from /health, got %q", BackendLlamaCPP, got.Backend)
	}
	if got.Version != "" {
		t.Fatalf("want empty version without /props, got %q", got.Version)
	}
}

func TestDetect_LlamaCPP_OpenAIModelsEndpointWithSize(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"llama-70b-q4","size":424242}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	got := Detect(context.Background(), Config{Endpoint: srv.URL + "/v1"}, srv.Client())
	if got.State != StateRunning {
		t.Fatalf("want Running, got %q", got.State)
	}
	if got.Backend != BackendLlamaCPP {
		t.Fatalf("want backend %q from OpenAI /v1/models, got %q", BackendLlamaCPP, got.Backend)
	}
	if len(got.Models) != 1 {
		t.Fatalf("models=%v, want one model", got.Models)
	}
	if got.Models[0].Name != "llama-70b-q4" || got.Models[0].Size != 424242 {
		t.Fatalf("model=%+v, want name llama-70b-q4 size 424242", got.Models[0])
	}
}

func TestDetect_LlamaCPP_ModelSizeFromLocalPropsPath(t *testing.T) {
	modelPath := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(modelPath, []byte("real-model-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"local-gguf"}]}`))
	})
	mux.HandleFunc("/props", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_slots":2,"model_path":` + strconvQuote(modelPath) + `}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	got := Detect(context.Background(), Config{Endpoint: srv.URL + "/v1"}, srv.Client())
	if len(got.Models) != 1 {
		t.Fatalf("models=%v, want one model", got.Models)
	}
	if got.Models[0].Size != int64(len("real-model-bytes")) {
		t.Fatalf("model size=%d, want stat size %d", got.Models[0].Size, len("real-model-bytes"))
	}
}

func TestDetect_GPUStack_NotRetaggedAsLlamaCPP(t *testing.T) {
	// A reachable GPUStack server (OpenAI alias only, no /health|/props) must
	// keep the gpustack tag — the llama.cpp fallback demands a positive
	// signal and must not misfire on GPUStack's 2xx OpenAI alias.
	srv := gpustackStub(t, true, "", "")
	got := Detect(context.Background(), Config{Endpoint: srv.URL}, srv.Client())
	if got.Backend != BackendGPUStack {
		t.Fatalf("GPUStack must not be retagged llamacpp; got %q", got.Backend)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
