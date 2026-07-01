package warm

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// fakeOllama is an in-memory OllamaControl.
type fakeOllama struct {
	mu       sync.Mutex
	loaded   map[string]bool
	onDisk   map[string]uint64 // model → size
	pulls    int
	pinned   []string
	released []string
}

func newFakeOllama() *fakeOllama {
	return &fakeOllama{loaded: map[string]bool{}, onDisk: map[string]uint64{}}
}

func (f *fakeOllama) EnsureResident(_ context.Context, model string, pull bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.onDisk[model]; !ok {
		if !pull {
			return errors.New("model not on disk")
		}
		f.pulls++
		f.onDisk[model] = 1 << 20
	}
	f.loaded[model] = true
	f.pinned = append(f.pinned, model)
	return nil
}

func (f *fakeOllama) Release(_ context.Context, model string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loaded[model] = false
	f.released = append(f.released, model)
	return nil
}

func (f *fakeOllama) ModelSizeBytes(_ context.Context, model string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.onDisk[model], nil
}

func (f *fakeOllama) LoadedModels(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for m, on := range f.loaded {
		if on {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeOllama) OnDisk(_ context.Context, model string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.onDisk[model]
	return ok
}

func (f *fakeOllama) isLoaded(model string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loaded[model]
}

// fakeGauge is a controllable LoadGauge.
type fakeGauge struct {
	busy   bool
	budget int64
}

func (g *fakeGauge) Busy() bool { return g.busy }
func (g *fakeGauge) WarmBudgetBytes(_ uint64) int64 {
	if g.busy {
		return 0
	}
	return g.budget
}

// fakeShard is a controllable ShardControl.
type fakeShard struct {
	active    string
	orchModel string
	stopped   int
}

func (s *fakeShard) ActiveModel() string { return s.active }
func (s *fakeShard) Orchestrate(_ context.Context, model string, _ int, _ []string) error {
	s.orchModel = model
	s.active = model
	return nil
}
func (s *fakeShard) Stop() { s.stopped++; s.active = "" }

func newExec(t *testing.T, ol *fakeOllama, g *fakeGauge, sh ShardControl) (*Executor, *[]string) {
	t.Helper()
	var persisted []string
	e := New(Config{
		Ollama:    ol,
		Shard:     sh,
		Gauge:     g,
		UsableMem: func() uint64 { return 32 << 30 },
		PersistDesired: func(s []string) error {
			persisted = append([]string(nil), s...)
			return nil
		},
	})
	return e, &persisted
}

func TestApplyLoadIdle(t *testing.T) {
	ol := newFakeOllama()
	ol.onDisk["llama3.2"] = 2 << 30
	g := &fakeGauge{budget: 10 << 30}
	e, persisted := newExec(t, ol, g, nil)

	resp, err := e.Apply(context.Background(), WarmRequest{Model: "llama3.2", Mode: ModeLoad})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if resp.Status != StatusLoaded {
		t.Fatalf("status = %q, want loaded", resp.Status)
	}
	if !ol.isLoaded("llama3.2") {
		t.Fatal("model should be resident")
	}
	if len(*persisted) != 1 || (*persisted)[0] != "llama3.2" {
		t.Fatalf("desired set not persisted: %v", *persisted)
	}
}

func TestApplyLoadBusySkips(t *testing.T) {
	ol := newFakeOllama()
	ol.onDisk["llama3.2"] = 2 << 30
	g := &fakeGauge{busy: true, budget: 10 << 30}
	e, _ := newExec(t, ol, g, nil)

	resp, err := e.Apply(context.Background(), WarmRequest{Model: "llama3.2", Mode: ModeLoad})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if resp.Status != StatusSkippedBusy {
		t.Fatalf("status = %q, want skipped_busy", resp.Status)
	}
	if resp.WarmBudgetBytes != 0 {
		t.Fatalf("busy budget = %d, want 0", resp.WarmBudgetBytes)
	}
	if ol.isLoaded("llama3.2") {
		t.Fatal("busy host must not load")
	}
	// Still remembered as desired so the supervisor restores it later.
	if d := e.Desired(); len(d) != 1 || d[0] != "llama3.2" {
		t.Fatalf("desired = %v, want [llama3.2]", d)
	}
}

func TestApplyLoadOverBudget(t *testing.T) {
	ol := newFakeOllama()
	ol.onDisk["huge"] = 20 << 30
	g := &fakeGauge{budget: 10 << 30}
	e, _ := newExec(t, ol, g, nil)

	resp, _ := e.Apply(context.Background(), WarmRequest{Model: "huge", Mode: ModeLoad})
	if resp.Status != StatusOverBudget {
		t.Fatalf("status = %q, want over_budget", resp.Status)
	}
	if ol.isLoaded("huge") {
		t.Fatal("over-budget model must not load")
	}
}

func TestApplyShard(t *testing.T) {
	ol := newFakeOllama()
	g := &fakeGauge{budget: 10 << 30}
	sh := &fakeShard{}
	e, _ := newExec(t, ol, g, sh)

	resp, err := e.Apply(context.Background(), WarmRequest{Model: "70b", Mode: ModeShard})
	if err != nil {
		t.Fatalf("shard: %v", err)
	}
	if resp.Status != StatusShardStarted {
		t.Fatalf("status = %q, want shard_started", resp.Status)
	}

	// Re-request the already-active model → already_active.
	sh.active = "70b"
	resp2, _ := e.Apply(context.Background(), WarmRequest{Model: "70b", Mode: ModeShard})
	if resp2.Status != StatusAlreadyActive {
		t.Fatalf("status = %q, want already_active", resp2.Status)
	}
}

func TestApplyShardUnavailable(t *testing.T) {
	ol := newFakeOllama()
	g := &fakeGauge{budget: 10 << 30}
	e, _ := newExec(t, ol, g, nil)
	_, err := e.Apply(context.Background(), WarmRequest{Model: "70b", Mode: ModeShard})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 503 {
		t.Fatalf("want 503 APIError, got %v", err)
	}
}

func TestApplyUnload(t *testing.T) {
	ol := newFakeOllama()
	ol.onDisk["llama3.2"] = 2 << 30
	ol.loaded["llama3.2"] = true
	g := &fakeGauge{budget: 10 << 30}
	sh := &fakeShard{active: "llama3.2"}
	e, _ := newExec(t, ol, g, sh)
	e.addDesired("llama3.2")

	resp, err := e.Apply(context.Background(), WarmRequest{Model: "llama3.2", Mode: ModeUnload})
	if err != nil {
		t.Fatalf("unload: %v", err)
	}
	if resp.Status != StatusUnloaded {
		t.Fatalf("status = %q, want unloaded", resp.Status)
	}
	if ol.isLoaded("llama3.2") {
		t.Fatal("model should be released")
	}
	if sh.stopped == 0 {
		t.Fatal("shard should be stopped for the unloaded active model")
	}
	if len(e.Desired()) != 0 {
		t.Fatalf("desired should be empty after unload, got %v", e.Desired())
	}
}

func TestApplyBadMode(t *testing.T) {
	ol := newFakeOllama()
	g := &fakeGauge{budget: 10 << 30}
	e, _ := newExec(t, ol, g, nil)
	_, err := e.Apply(context.Background(), WarmRequest{Model: "x", Mode: "bogus"})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("want 400 APIError, got %v", err)
	}
	_, err = e.Apply(context.Background(), WarmRequest{Model: "", Mode: ModeLoad})
	if !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("empty model: want 400, got %v", err)
	}
}

func TestSupervisorYieldAndRestore(t *testing.T) {
	ol := newFakeOllama()
	ol.onDisk["a"] = 1 << 30
	ol.onDisk["b"] = 1 << 30
	ol.loaded["a"] = true
	ol.loaded["b"] = true
	g := &fakeGauge{budget: 10 << 30}
	e, _ := newExec(t, ol, g, nil)
	e.addDesired("a")
	e.addDesired("b")

	// Busy → yield: both released.
	g.busy = true
	e.reconcile(context.Background())
	if ol.isLoaded("a") || ol.isLoaded("b") {
		t.Fatal("busy reconcile should release the warm set")
	}

	// Idle → restore: both back resident (no pull needed, on disk).
	g.busy = false
	e.reconcile(context.Background())
	if !ol.isLoaded("a") || !ol.isLoaded("b") {
		t.Fatal("idle reconcile should restore the warm set")
	}
}

func TestSupervisorRestoreSkipsOverBudget(t *testing.T) {
	ol := newFakeOllama()
	ol.onDisk["big"] = 20 << 30
	g := &fakeGauge{budget: 10 << 30}
	e, _ := newExec(t, ol, g, nil)
	e.addDesired("big")

	g.busy = false
	e.reconcile(context.Background())
	if ol.isLoaded("big") {
		t.Fatal("restore must skip an over-budget model")
	}
}
