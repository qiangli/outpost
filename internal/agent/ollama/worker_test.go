package ollama

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const workerTagsBody = `{"models":[{"name":"llama3.2:1b","digest":"d1","size":100}]}`

// runOnePush drives the watcher until the first registry push lands.
func runOnePush(t *testing.T, w *Watcher, reg *capturingRegistry) RegistryPushPayload {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && reg.calls.Load() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	p, ok := reg.lastPayload()
	if !ok {
		t.Fatal("no registry push captured")
	}
	return p
}

func rawHasWorkerKey(t *testing.T, reg *capturingRegistry) bool {
	t.Helper()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(reg.raws[len(reg.raws)-1], &m); err != nil {
		t.Fatalf("decode raw push: %v", err)
	}
	_, ok := m["worker"]
	return ok
}

func TestWatcher_WorkerBlockOnProvisionedGPUWorker(t *testing.T) {
	reg := &capturingRegistry{}
	w, _, _ := newTestWatcher(t, &stubTags{bodies: []string{workerTagsBody}}, reg, nil)
	w.cfg.WorkerRequestID = "req-0123"
	w.cfg.WorkerGPU = func(context.Context) (int64, string, error) {
		return 80 << 30, "NVIDIA A100-SXM4-80GB", nil
	}

	p := runOnePush(t, w, reg)
	if p.Worker == nil {
		t.Fatal("worker block missing on a provisioned GPU worker push")
	}
	if p.Worker.RequestID != "req-0123" || p.Worker.VRAMBytes != 80<<30 || p.Worker.GPUModel != "NVIDIA A100-SXM4-80GB" {
		t.Errorf("worker=%+v", *p.Worker)
	}
	if len(p.Models) != 1 {
		t.Errorf("models=%d, want the cached models as today", len(p.Models))
	}
}

func TestWatcher_NoWorkerBlockOnOrdinaryHost(t *testing.T) {
	reg := &capturingRegistry{}
	w, _, _ := newTestWatcher(t, &stubTags{bodies: []string{workerTagsBody}}, reg, nil)
	called := false
	w.cfg.WorkerGPU = func(context.Context) (int64, string, error) {
		called = true
		return 1, "x", nil
	}
	runOnePush(t, w, reg)
	if rawHasWorkerKey(t, reg) {
		t.Error("non-worker host emitted a worker key")
	}
	if called {
		t.Error("non-worker host measured the GPU")
	}
}

func TestWatcher_WorkerBlockOmittedWhenUnmeasurable(t *testing.T) {
	reg := &capturingRegistry{}
	w, _, _ := newTestWatcher(t, &stubTags{bodies: []string{workerTagsBody}}, reg, nil)
	w.cfg.WorkerRequestID = "req-0123"
	w.cfg.WorkerGPU = func(context.Context) (int64, string, error) {
		return 0, "", errNoGPU
	}
	p := runOnePush(t, w, reg)
	if rawHasWorkerKey(t, reg) {
		t.Errorf("unmeasurable GPU still emitted worker=%+v", p.Worker)
	}
	if len(p.Models) != 1 {
		t.Errorf("push must still carry models; got %d", len(p.Models))
	}
}

func TestWatcher_WorkerBlockOmittedForInvalidRequestID(t *testing.T) {
	reg := &capturingRegistry{}
	w, _, _ := newTestWatcher(t, &stubTags{bodies: []string{workerTagsBody}}, reg, nil)
	w.cfg.WorkerRequestID = strings.Repeat("r", 65)
	w.cfg.WorkerGPU = func(context.Context) (int64, string, error) { return 1 << 30, "gpu", nil }
	runOnePush(t, w, reg)
	if rawHasWorkerKey(t, reg) {
		t.Error("over-long request_id must omit the block (the master would 400 the whole push)")
	}
}

func TestBuildWorkerInfo_ClampsGPUModel(t *testing.T) {
	long := strings.Repeat("é", 40) // 80 bytes
	wi, err := buildWorkerInfo("req", 1, long)
	if err != nil {
		t.Fatal(err)
	}
	if len(wi.GPUModel) > maxWorkerGPUModelLen || !strings.HasPrefix(long, wi.GPUModel) {
		t.Errorf("gpu_model=%q (len %d) not clamped on a rune boundary", wi.GPUModel, len(wi.GPUModel))
	}
	if _, err := buildWorkerInfo("req", -1, "gpu"); err == nil {
		t.Error("negative vram must be refused")
	}
	if _, err := buildWorkerInfo("", 1, "gpu"); err == nil {
		t.Error("empty request_id must be refused")
	}
}

func TestParseNvidiaSmiGPUs(t *testing.T) {
	vram, model, err := parseNvidiaSmi("NVIDIA L4, 23034\nNVIDIA L4, 23034\n")
	if err != nil {
		t.Fatal(err)
	}
	if vram != 2*23034<<20 || model != "NVIDIA L4" {
		t.Errorf("vram=%d model=%q", vram, model)
	}
	if _, _, err := parseNvidiaSmi(""); err == nil {
		t.Error("empty output must be unmeasurable")
	}
}
