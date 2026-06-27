package clusterllm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// gpustackStub stands in for a GPUStack server. reachable controls whether
// the OpenAI alias answers; devices is the gpu-devices payload (nil ⇒ 404
// on every management path, the older-backend / no-key fallback case).
func gpustackStub(t *testing.T, reachable bool, devicesJSON string, wantKey string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1-openai/models", func(w http.ResponseWriter, r *http.Request) {
		if !reachable {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		// GPUStack requires auth even here; a 401 must still read as
		// "reachable". Return 401 when no/incorrect key to exercise that.
		if wantKey != "" && r.Header.Get("Authorization") != "Bearer "+wantKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	})
	mux.HandleFunc("/v2/gpu-devices", func(w http.ResponseWriter, r *http.Request) {
		if devicesJSON == "" || r.Header.Get("Authorization") != "Bearer "+wantKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(devicesJSON))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDetect_Unconfigured(t *testing.T) {
	got := Detect(context.Background(), Config{}, nil)
	if got.State != StateUnconfigured {
		t.Fatalf("empty endpoint: want StateUnconfigured, got %q", got.State)
	}
}

func TestDetect_NotReachable(t *testing.T) {
	// A closed server: nothing listening on the endpoint.
	srv := gpustackStub(t, false, "", "")
	got := Detect(context.Background(), Config{Endpoint: srv.URL}, srv.Client())
	// reachable=false returns 502 on the alias, which is still an HTTP
	// status > 0 ⇒ Running by our "any status proves a daemon" rule. To
	// test true NotReachable we point at a dead address instead.
	if got.State != StateRunning {
		t.Fatalf("502 from a live server should read as Running, got %q", got.State)
	}
	dead := Detect(context.Background(), Config{Endpoint: "http://127.0.0.1:1"}, srv.Client())
	if dead.State != StateNotReachable {
		t.Fatalf("dead endpoint: want StateNotReachable, got %q", dead.State)
	}
}

func TestDetect_RunningNoKey(t *testing.T) {
	// Reachable but no API key ⇒ Running, MemberCount 1, no VRAM (filter
	// stays inert). This is the bench "single-node, wiring-only" case.
	srv := gpustackStub(t, true, "", "")
	got := Detect(context.Background(), Config{Endpoint: srv.URL}, srv.Client())
	if got.State != StateRunning {
		t.Fatalf("want Running, got %q", got.State)
	}
	if got.MemberCount != 1 {
		t.Fatalf("want MemberCount 1 (reachable=one node), got %d", got.MemberCount)
	}
	if got.AggregateVRAMBytes != 0 {
		t.Fatalf("want 0 VRAM without a key (inert filter), got %d", got.AggregateVRAMBytes)
	}
	if got.Backend != BackendGPUStack {
		t.Fatalf("want default backend %q, got %q", BackendGPUStack, got.Backend)
	}
}

func TestDetect_RunningWithKey_AggregatesVRAM(t *testing.T) {
	// Two workers, three devices: 24+36 GiB on one Mac, 24 GiB on another.
	const giB = 1 << 30
	devices := `{"items":[
		{"worker_name":"host-a","memory":{"total":25769803776,"is_unified_memory":true}},
		{"worker_name":"host-c","memory":{"total":38654705664,"is_unified_memory":true}},
		{"worker_name":"host-c","memory":{"total":25769803776,"is_unified_memory":true}}
	]}`
	srv := gpustackStub(t, true, devices, "secret-key")
	got := Detect(context.Background(), Config{Endpoint: srv.URL, APIKey: "secret-key"}, srv.Client())
	if got.State != StateRunning {
		t.Fatalf("want Running, got %q", got.State)
	}
	if got.MemberCount != 2 {
		t.Fatalf("want 2 distinct workers, got %d", got.MemberCount)
	}
	wantVRAM := uint64(24*giB + 36*giB + 24*giB)
	if got.AggregateVRAMBytes != wantVRAM {
		t.Fatalf("want summed VRAM %d, got %d", wantVRAM, got.AggregateVRAMBytes)
	}
}

func TestDetect_WrongKey_InertFallback(t *testing.T) {
	// Reachable, but the management API rejects the key ⇒ Running with the
	// inert fallback (no VRAM), never an error or a hidden host.
	srv := gpustackStub(t, true, `{"items":[{"memory":{"total":1}}]}`, "real-key")
	got := Detect(context.Background(), Config{Endpoint: srv.URL, APIKey: "wrong-key"}, srv.Client())
	if got.State != StateRunning {
		t.Fatalf("want Running, got %q", got.State)
	}
	if got.AggregateVRAMBytes != 0 || got.MemberCount != 1 {
		t.Fatalf("auth-rejected management API must stay inert; got members=%d vram=%d",
			got.MemberCount, got.AggregateVRAMBytes)
	}
}

func TestDetector_Caches(t *testing.T) {
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc("/v1-openai/models", func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	d := NewDetector(Config{Endpoint: srv.URL}, 0, srv.Client())
	_ = d.Info(context.Background())
	_ = d.Info(context.Background())
	if hits != 1 {
		t.Fatalf("second Info() should hit the TTL cache; probes=%d", hits)
	}
}
