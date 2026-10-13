package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/shard"
)

// recordingLeadManager is a hermetic shardLeadManager: it captures the model
// and API port each trigger verb carries instead of launching prima or
// dialing the mesh.
type recordingLeadManager struct {
	mu sync.Mutex

	orchModel string
	orchPort  int
	orchCalls int

	leadPeer  shard.ShardPeer
	leadModel string
	leadPort  int
	leadCalls int
}

func newRecordingLeadManager() *recordingLeadManager {
	return &recordingLeadManager{}
}

func (f *recordingLeadManager) Orchestrate(_ context.Context, model string, apiPort int, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orchModel, f.orchPort = model, apiPort
	f.orchCalls++
	return nil
}

func (f *recordingLeadManager) TellLead(_ context.Context, peer shard.ShardPeer, model string, apiPort int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leadPeer, f.leadModel, f.leadPort = peer, model, apiPort
	f.leadCalls++
	return nil
}

func (f *recordingLeadManager) orchSnapshot() (model string, port, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.orchModel, f.orchPort, f.orchCalls
}

func (f *recordingLeadManager) leadSnapshot() (peer shard.ShardPeer, model string, port, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leadPeer, f.leadModel, f.leadPort, f.leadCalls
}

// waitOrchestrate blocks until the backgrounded self-lead reaches the fake
// (the local path returns before Orchestrate runs).
func waitOrchestrate(t *testing.T, f *recordingLeadManager, wantCalls int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, _, calls := f.orchSnapshot()
		if calls >= wantCalls {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for Orchestrate call %d", wantCalls)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func closedLoopbackURL() string { return "http://127.0.0.1:1" } // refused fast, no listener

// The trigger must serve on the configured ollama unit's port, not a pinned
// literal: changing the unit target moves both the self-lead (Orchestrate)
// and the peer-lead (TellLead) ports.
func TestRunShardTriggerFollowsChangedOllamaPort(t *testing.T) {
	t.Setenv("OLLAMA_HOST", closedLoopbackURL())
	t.Setenv("BASHY_OLLAMA_PORT", "0") // ephemeral: no stable engine URL to compete

	unitURL := "http://127.0.0.1:12111"
	unitTarget := func() string { return unitURL }
	resolvePort := func() (int, error) { return resolveShardAPIPort(unitTarget) }
	resolvePeer := func(_ context.Context, host string) (shard.ShardPeer, error) {
		return shard.ShardPeer{Host: host, PeerID: "peer-id-" + host}, nil
	}

	fake := newRecordingLeadManager()
	ctx := context.Background()

	if err := runShardTrigger(ctx, "self", "m1", fake, resolvePeer, resolvePort); err != nil {
		t.Fatalf("self trigger: %v", err)
	}
	waitOrchestrate(t, fake, 1)
	if model, port, _ := fake.orchSnapshot(); model != "m1" || port != 12111 {
		t.Errorf("Orchestrate(model, port) = (%q, %d), want (m1, 12111)", model, port)
	}

	if err := runShardTrigger(ctx, "peer1", "m1", fake, resolvePeer, resolvePort); err != nil {
		t.Fatalf("peer trigger: %v", err)
	}
	if peer, model, port, _ := fake.leadSnapshot(); peer.Host != "peer1" || model != "m1" || port != 12111 {
		t.Errorf("TellLead(peer, model, port) = (%+v, %q, %d), want (peer1, m1, 12111)", peer, model, port)
	}

	// Retarget the ollama unit: the next triggers must follow it with no
	// restart and no code change.
	unitURL = "http://127.0.0.1:12222"

	if err := runShardTrigger(ctx, "local", "m2", fake, resolvePeer, resolvePort); err != nil {
		t.Fatalf("retargeted self trigger: %v", err)
	}
	waitOrchestrate(t, fake, 2)
	if model, port, _ := fake.orchSnapshot(); model != "m2" || port != 12222 {
		t.Errorf("after retarget Orchestrate(model, port) = (%q, %d), want (m2, 12222)", model, port)
	}

	if err := runShardTrigger(ctx, "peer2", "m2", fake, resolvePeer, resolvePort); err != nil {
		t.Fatalf("retargeted peer trigger: %v", err)
	}
	if peer, model, port, _ := fake.leadSnapshot(); peer.Host != "peer2" || model != "m2" || port != 12222 {
		t.Errorf("after retarget TellLead(peer, model, port) = (%+v, %q, %d), want (peer2, m2, 12222)", peer, model, port)
	}
}

// With the ollama unit down, the trigger serves on bashy's engine port
// rather than a dead loopback.
func TestRunShardTriggerFallsBackToBashyEnginePort(t *testing.T) {
	t.Setenv("OLLAMA_HOST", closedLoopbackURL()) // refused fast
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer engine.Close()
	u, err := url.Parse(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_OLLAMA_PORT", port)

	want, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	// The unit contributes nothing: the resolver must reach the engine.
	unitTarget := func() string { return "" }
	resolvePort := func() (int, error) { return resolveShardAPIPort(unitTarget) }

	fake := newRecordingLeadManager()
	resolvePeer := func(_ context.Context, host string) (shard.ShardPeer, error) {
		return shard.ShardPeer{Host: host, PeerID: "p"}, nil
	}
	if err := runShardTrigger(context.Background(), "peer9", "m", fake, resolvePeer, resolvePort); err != nil {
		t.Fatalf("peer trigger: %v", err)
	}
	if _, _, got, _ := fake.leadSnapshot(); got != want {
		t.Errorf("TellLead port = %d, want bashy engine port %d", got, want)
	}
}

// With no endpoint anywhere, the trigger refuses instead of serving a
// guessed port.
func TestRunShardTriggerWithoutEndpointRefuses(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "")
	t.Setenv("BASHY_OLLAMA_PORT", "0")
	resolvePort := func() (int, error) { return resolveShardAPIPort(nil) }

	fake := newRecordingLeadManager()
	resolvePeer := func(_ context.Context, host string) (shard.ShardPeer, error) {
		return shard.ShardPeer{Host: host, PeerID: "p"}, nil
	}
	if err := runShardTrigger(context.Background(), "peer9", "m", fake, resolvePeer, resolvePort); err == nil {
		t.Fatal("peer trigger with no endpoint must fail, got nil")
	}
	if err := runShardTrigger(context.Background(), "self", "m", fake, resolvePeer, resolvePort); err == nil {
		t.Fatal("self trigger with no endpoint must fail, got nil")
	}
	if _, _, _, calls := fake.leadSnapshot(); calls != 0 {
		t.Errorf("TellLead ran %d times with no endpoint, want 0", calls)
	}
}

// A nil manager keeps the unit-inactive refusal (typed-nil safe: the daemon
// checks before wrapping, and direct callers get the same answer).
func TestRunShardTriggerNilManagerRefuses(t *testing.T) {
	resolvePeer := func(_ context.Context, host string) (shard.ShardPeer, error) {
		return shard.ShardPeer{Host: host, PeerID: "p"}, nil
	}
	resolvePort := func() (int, error) { return 1, nil }
	if err := runShardTrigger(context.Background(), "self", "m", nil, resolvePeer, resolvePort); err != errShardUnitInactive {
		t.Errorf("nil manager error = %v, want the unit-inactive refusal", err)
	}
}

// The port resolver itself: unit target wins, then the engine, then refusal.
func TestResolveShardAPIPortUnitTargetWins(t *testing.T) {
	t.Setenv("OLLAMA_HOST", closedLoopbackURL())
	t.Setenv("BASHY_OLLAMA_PORT", "12099")
	got, err := resolveShardAPIPort(func() string { return "http://unit-target:12311" })
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != 12311 {
		t.Errorf("port = %d, want the unit target's 12311", got)
	}
}

func TestResolveShardAPIPortWithoutEndpoint(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "")
	t.Setenv("BASHY_OLLAMA_PORT", "0")
	if _, err := resolveShardAPIPort(nil); err == nil {
		t.Error("no endpoint must fail, got nil")
	}
}
