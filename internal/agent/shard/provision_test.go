package shard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// freshMeshPair wires two managers over a REAL in-process mesh: a leader with
// a pre-set candidate ring and a worker serving the control endpoint with
// stubbed form handling. Nothing launches prima and nothing downloads: the
// worker's EnsureBins fake stands in for the binmgr-verified fetch.
type freshMeshPair struct {
	leader *Manager
	worker *Manager

	workerServerBin string
	workerWorkerBin string

	ensureCalls chan struct{}
	ensureErr   error

	leaderFormed chan int
	workerFormed chan formedRank
}

type formedRank struct {
	rank  int
	model string
	world int
}

func newFreshMeshPair(t *testing.T) (*freshMeshPair, func()) {
	t.Helper()

	workerHost := newMeshHost(t)
	leaderHost := newMeshHost(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := leaderHost.LibP2PHost().Connect(ctx, peer.AddrInfo{
		ID:    workerHost.LibP2PHost().ID(),
		Addrs: workerHost.LibP2PHost().Addrs(),
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	p := &freshMeshPair{
		ensureCalls:  make(chan struct{}, 16),
		leaderFormed: make(chan int, 1),
		workerFormed: make(chan formedRank, 1),
	}
	binDir := t.TempDir() // missing files: a fresh host, nothing provisioned
	p.workerServerBin = filepath.Join(binDir, "llama-server")
	p.workerWorkerBin = filepath.Join(binDir, "llama-cli")

	p.worker = NewManager(ManagerConfig{
		Self:      ShardPeer{Host: "worker", PeerID: workerHost.PeerID()},
		Forwarder: workerHost.Forwarder(),
		Peers:     &fakePeers{},
		Bins:      ServeBins{ServerBin: p.workerServerBin, WorkerBin: p.workerWorkerBin},
		EnsureBins: func(context.Context) error {
			p.ensureCalls <- struct{}{}
			if p.ensureErr != nil {
				// A binmgr signature rejection: no bytes land, the gate
				// must stay closed.
				return p.ensureErr
			}
			for _, dst := range []string{p.workerServerBin, p.workerWorkerBin} {
				if err := os.WriteFile(dst, []byte("signed-prima"), 0o755); err != nil {
					return err
				}
			}
			return nil
		},
	})
	p.worker.onForm = func(_ context.Context, ring *Ring, rank int, sc ServeConfig) error {
		p.workerFormed <- formedRank{rank: rank, model: sc.Model, world: len(ring.Members)}
		return nil
	}
	cleanup, err := p.worker.ServeControl()
	if err != nil {
		t.Fatal(err)
	}

	p.leader = NewManager(ManagerConfig{
		Self:      ShardPeer{Host: "leader", PeerID: leaderHost.PeerID()},
		Forwarder: leaderHost.Forwarder(),
		Peers:     &fakePeers{},
	})
	p.leader.mu.Lock()
	p.leader.ring = &Ring{Members: []Member{
		{Rank: 0, Host: "leader", PeerID: leaderHost.PeerID()},
		{Rank: 1, Host: "worker", PeerID: workerHost.PeerID()},
	}}
	p.leader.mu.Unlock()
	// The real PingPeer over the mesh: readiness is read from the worker's
	// disk, so the test observes the binaries actually landing.
	p.leader.provisionPoll = 20 * time.Millisecond
	p.leader.provisionWait = 30 * time.Second
	p.leader.onForm = func(_ context.Context, _ *Ring, rank int, _ ServeConfig) error {
		if rank == 0 {
			p.leaderFormed <- rank
		}
		return nil
	}

	return p, func() {
		cleanup()
		workerHost.Close()
		leaderHost.Close()
	}
}

// A fresh mesh — no prima on disk anywhere — forms on the FIRST trigger: the
// leader's pre-filter /provision ask lands the worker's engine, the bounded
// poll observes it, the readiness gate keeps the worker, and both ranks form.
func TestOrchestrate_FreshHostsFormOnFirstTrigger(t *testing.T) {
	p, done := newFreshMeshPair(t)
	defer done()

	if _, err := os.Stat(p.workerWorkerBin); !os.IsNotExist(err) {
		t.Fatal("worker binary must be absent before the trigger (fresh host)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := p.leader.Orchestrate(ctx, "big-model", 11434, nil); err != nil {
		t.Fatalf("Orchestrate on fresh hosts: %v", err)
	}

	select {
	case <-p.ensureCalls:
	default:
		t.Error("worker was never asked to provision (no /provision round before the filter)")
	}
	if _, err := os.Stat(p.workerWorkerBin); err != nil {
		t.Errorf("worker binary missing after form: %v", err)
	}
	select {
	case <-p.leaderFormed:
	case <-time.After(5 * time.Second):
		t.Error("leader did not form its own rank 0")
	}
	select {
	case f := <-p.workerFormed:
		if f.rank != 1 {
			t.Errorf("worker rank = %d, want 1", f.rank)
		}
		if f.model != "big-model" {
			t.Errorf("worker model = %q, want big-model", f.model)
		}
		if f.world != 2 {
			t.Errorf("worker world = %d, want 2 (worker kept by the gate)", f.world)
		}
	case <-time.After(5 * time.Second):
		t.Error("worker never received its /form (gate dropped it despite provisioning)")
	}
}

// A signature rejection stays fail-closed: the worker's engine never lands,
// the gate drops it, and the form aborts without standing up EITHER rank —
// no unverified fallback, no half-formed leader serving alone.
func TestOrchestrate_SignatureFailureFailsClosed(t *testing.T) {
	p, done := newFreshMeshPair(t)
	defer done()
	p.ensureErr = errors.New("sigstore: no admitted bashy-built, signed release")
	p.leader.provisionWait = 300 * time.Millisecond

	leaderFormed := false
	p.leader.onForm = func(_ context.Context, _ *Ring, _ int, _ ServeConfig) error {
		leaderFormed = true
		return nil
	}
	workerFormed := false
	p.worker.onForm = func(_ context.Context, _ *Ring, _ int, _ ServeConfig) error {
		workerFormed = true
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := p.leader.Orchestrate(ctx, "big-model", 11434, nil)
	if err == nil {
		t.Fatal("Orchestrate with an unprovisionable worker must fail, got nil")
	}
	if !strings.Contains(err.Error(), "shard-ready") {
		t.Errorf("error = %q, want the shard-ready gate refusal", err.Error())
	}
	// The backgrounded /form path must not have fired either: the worker only
	// forms via tellWorker after the gate, and the leader only after that.
	time.Sleep(200 * time.Millisecond)
	if workerFormed {
		t.Error("worker formed despite the signature failure")
	}
	if leaderFormed {
		t.Error("leader formed alone despite the signature failure (half-formed shard)")
	}
	if _, statErr := os.Stat(p.workerWorkerBin); !os.IsNotExist(statErr) {
		t.Error("rejected bytes must not land on disk")
	}
}

// TellLead naming this node — by mesh peer id or by host label — never
// mesh-dials itself (EOF on the loopback forward): it leads locally. Anything
// else still rides the mesh, and a mesh failure still errors.
func TestTellLead_SelfShortCircuits(t *testing.T) {
	fw := newFake()
	m := NewManager(ManagerConfig{
		Self: ShardPeer{Host: "me", PeerID: "pid-me"}, Forwarder: fw, Peers: &fakePeers{},
	})
	var mu sync.Mutex
	var calls [][2]any
	orchestrated := make(chan struct{}, 16)
	m.orchestrate = func(_ context.Context, model string, port int, _ []string) error {
		mu.Lock()
		calls = append(calls, [2]any{model, port})
		mu.Unlock()
		orchestrated <- struct{}{}
		return nil
	}
	waitLead := func() [2]any {
		t.Helper()
		select {
		case <-orchestrated:
		case <-time.After(5 * time.Second):
			t.Fatal("self-trigger never reached the local orchestrate")
		}
		mu.Lock()
		defer mu.Unlock()
		return calls[len(calls)-1]
	}

	// By peer id (what cloudbox host→peer-id resolution returns for our own host).
	if err := m.TellLead(context.Background(), ShardPeer{Host: "me", PeerID: "pid-me"}, "m1", 11434); err != nil {
		t.Fatalf("TellLead self by peer id: %v", err)
	}
	if got := waitLead(); got != ([2]any{"m1", 11434}) {
		t.Errorf("self-lead by peer id orchestrated %v, want [m1 11434]", got)
	}

	// By host label alone (a stale rendezvous id must not force a self-dial).
	if err := m.TellLead(context.Background(), ShardPeer{Host: "me", PeerID: "stale-id"}, "m2", 11435); err != nil {
		t.Fatalf("TellLead self by host: %v", err)
	}
	if got := waitLead(); got != ([2]any{"m2", 11435}) {
		t.Errorf("self-lead by host orchestrated %v, want [m2 11435]", got)
	}

	if fw.n != 0 {
		t.Errorf("self-trigger dialed the mesh %d time(s), want 0", fw.n)
	}

	// A genuinely remote peer still rides the mesh: a forwarder failure
	// errors out and never touches the local orchestrate.
	fw.failAt = fw.n + 1
	nBefore := len(calls)
	if err := m.TellLead(context.Background(), ShardPeer{Host: "peer", PeerID: "pid-peer"}, "m3", 11434); err == nil {
		t.Error("TellLead to an unreachable peer must fail, got nil")
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != nBefore {
		t.Errorf("remote TellLead failure orchestrated locally %d time(s), want 0", len(calls)-nBefore)
	}
}

// Activation provisions the signed engine in the background: Run drives
// EnsureBins without blocking discovery, so the node is shard-ready by
// first trigger.
func TestRun_ActivationProvisionsBins(t *testing.T) {
	provisioned := make(chan struct{}, 1)
	m := NewManager(ManagerConfig{
		Self: ShardPeer{Host: "solo", PeerID: "pid"}, Peers: &fakePeers{},
		EnsureBins: func(context.Context) error {
			provisioned <- struct{}{}
			return nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()
	select {
	case <-provisioned:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not drive activation-time provisioning")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on ctx cancel")
	}
}
