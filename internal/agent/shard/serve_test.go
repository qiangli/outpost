package shard

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

func TestLaunchConfigFor_LeaderAndWorker(t *testing.T) {
	sc := ServeConfig{
		Model:     "/m/big.gguf",
		ServerBin: "/bin/llama-server",
		WorkerBin: "/bin/llama-cli",
		APIPort:   11434,
		Extra:     []string{"--prefetch"},
	}
	// Leader (rank 0): server binary + --host/--port + the model.
	lp, _ := ring3().PlanFor(0)
	lc := lp.LaunchConfigFor(sc)
	if lc.BinaryPath != "/bin/llama-server" {
		t.Errorf("leader binary = %q, want llama-server", lc.BinaryPath)
	}
	largv := strings.Join(lp.FullArgs(lc.ModelPath, lc.Extra...), " ")
	for _, want := range []string{"--rank 0", "-m /m/big.gguf", "--host 127.0.0.1", "--port 11434", "--prefetch"} {
		if !strings.Contains(largv, want) {
			t.Errorf("leader argv missing %q: %s", want, largv)
		}
	}
	// Worker (rank 1): worker binary, NO API host/port.
	wp, _ := ring3().PlanFor(1)
	wc := wp.LaunchConfigFor(sc)
	if wc.BinaryPath != "/bin/llama-cli" {
		t.Errorf("worker binary = %q, want llama-cli", wc.BinaryPath)
	}
	wargv := strings.Join(wp.FullArgs(wc.ModelPath, wc.Extra...), " ")
	if strings.Contains(wargv, "--port") || strings.Contains(wargv, "--host") {
		t.Errorf("worker must not serve an API: %s", wargv)
	}
	if !strings.Contains(wargv, "--rank 1") {
		t.Errorf("worker argv missing --rank 1: %s", wargv)
	}
}

func TestManager_FormAndAdvertise(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is unix-only")
	}
	f := newFake()
	m := NewManager(ManagerConfig{
		Self:      ShardPeer{Host: "leader", PeerID: "self"},
		Forwarder: f,
		Peers:     &fakePeers{},
	})
	if m.ActiveModel() != "" {
		t.Fatal("no model should be active before Form")
	}

	ring := ring2()
	sc := ServeConfig{Model: "qwen-72b", ServerBin: writeStub(t), WorkerBin: "/bin/true", APIPort: 9999}
	if err := m.Form(context.Background(), &ring, 0, sc); err != nil {
		t.Fatalf("Form: %v", err)
	}
	if m.ActiveModel() != "qwen-72b" {
		t.Errorf("ActiveModel = %q, want qwen-72b (the name the pool advertises)", m.ActiveModel())
	}
	// The leader's mesh wiring is up (rank 0 → 2 exposes, 2 forwards).
	if len(f.exposed) != 2 || len(f.opened) != 2 {
		t.Errorf("shard not wired: exposed=%d opened=%d", len(f.exposed), len(f.opened))
	}

	m.Stop()
	if m.ActiveModel() != "" {
		t.Errorf("ActiveModel should clear after Stop, got %q", m.ActiveModel())
	}
	if len(f.exposed) != 0 {
		t.Errorf("Stop should unexpose the mesh wiring: %v", f.exposed)
	}
}
