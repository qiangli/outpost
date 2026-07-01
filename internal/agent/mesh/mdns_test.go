package mesh

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// TestMDNSNotifeeSelfSkip verifies HandlePeerFound never tries to dial our own
// advertisement: passing our own peer ID must be a no-op (no panic, no dial).
// We construct a real (loopback) mesh host but feed the notifee the host's own
// ID with no addresses — if the self-skip branch were missing, host.Connect
// would be invoked with an empty addr set.
func TestMDNSNotifeeSelfSkip(t *testing.T) {
	h := newTestHost(t)
	defer h.Close()

	n := &mdnsNotifee{h: h.LibP2PHost(), log: silentLogger()}
	// Self: same ID as the host. No addrs — a dial attempt would error, but the
	// self-check must short-circuit before Connect is ever reached.
	n.HandlePeerFound(peer.AddrInfo{ID: h.LibP2PHost().ID()})

	// Still exactly zero peers — nothing was dialed.
	if got := h.Status().ConnectedPeers; got != 0 {
		t.Fatalf("self-skip should not connect anyone; connected peers = %d", got)
	}
}

// TestMDNSNotifeeAlreadyConnectedSkip verifies HandlePeerFound is a no-op for a
// peer we are already connected to (the Connectedness == Connected branch). We
// wire two real hosts, connect them directly, then hand the first host's
// notifee an AddrInfo for the second WITH NO ADDRESSES. If the
// already-connected guard were missing, Connect would run against an empty addr
// set and error; with the guard it returns immediately and the existing single
// connection is untouched.
func TestMDNSNotifeeAlreadyConnectedSkip(t *testing.T) {
	h1 := newTestHost(t)
	defer h1.Close()
	h2 := newTestHost(t)
	defer h2.Close()

	ctx := t.Context()
	ai := peer.AddrInfo{ID: h2.LibP2PHost().ID(), Addrs: h2.LibP2PHost().Addrs()}
	if err := h1.LibP2PHost().Connect(ctx, ai); err != nil {
		t.Fatalf("pre-connect h1->h2: %v", err)
	}
	if got := h1.Status().ConnectedPeers; got != 1 {
		t.Fatalf("setup: h1 connected peers = %d, want 1", got)
	}

	n := &mdnsNotifee{h: h1.LibP2PHost(), log: silentLogger()}
	// Discover h2 again but advertise no addrs: the already-connected branch
	// must short-circuit before any (failing) dial.
	n.HandlePeerFound(peer.AddrInfo{ID: h2.LibP2PHost().ID()})

	if got := h1.Status().ConnectedPeers; got != 1 {
		t.Fatalf("already-connected skip changed connections; connected peers = %d, want 1", got)
	}
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeMDNS is a stand-in for a libp2p mdns.Service so the supervisor's restart
// logic can be exercised without real multicast. startErr (if set) fails Start.
type fakeMDNS struct {
	mu       sync.Mutex
	started  int
	closed   int
	startErr error
}

func (f *fakeMDNS) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started++
	return nil
}

func (f *fakeMDNS) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeMDNS) counts() (started, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started, f.closed
}

// newHostForMDNS builds a Host wired to a factory-controlled fake mDNS, without
// standing up a real libp2p host (we only exercise the mDNS lifecycle).
func newHostForMDNS(factory func() (mdnsService, error)) *Host {
	return &Host{log: silentLogger(), newMDNS: factory}
}

// TestRestartMDNSSwapsService verifies restartMDNS starts a fresh service and,
// on a second call, closes the previous one before starting the replacement —
// the core of recovering a silently-dead LAN discovery service.
func TestRestartMDNSSwapsService(t *testing.T) {
	var svcs []*fakeMDNS
	m := newHostForMDNS(func() (mdnsService, error) {
		f := &fakeMDNS{}
		svcs = append(svcs, f)
		_ = f.Start()
		return f, nil
	})

	if err := m.restartMDNS(); err != nil {
		t.Fatalf("first restart: %v", err)
	}
	if !m.mdnsHealthy() {
		t.Fatal("service should be healthy after first start")
	}
	if err := m.restartMDNS(); err != nil {
		t.Fatalf("second restart: %v", err)
	}
	if len(svcs) != 2 {
		t.Fatalf("expected 2 services built, got %d", len(svcs))
	}
	if _, closed := svcs[0].counts(); closed != 1 {
		t.Errorf("first service should have been closed on restart; closed=%d", closed)
	}
	if started, _ := svcs[1].counts(); started != 1 {
		t.Errorf("replacement service should be started; started=%d", started)
	}
}

// TestRestartMDNSFactoryError verifies a factory failure leaves the host with no
// active service (so the supervisor knows to retry), rather than a stale one.
func TestRestartMDNSFactoryError(t *testing.T) {
	m := newHostForMDNS(func() (mdnsService, error) {
		return nil, errors.New("multicast down")
	})
	if err := m.restartMDNS(); err == nil {
		t.Fatal("expected factory error")
	}
	if m.mdnsHealthy() {
		t.Fatal("no service should be active after a factory failure")
	}
}

// TestCloseMDNSDisablesRestart verifies closeMDNS tears down the active service
// and prevents further (re)starts — clean shutdown.
func TestCloseMDNSDisablesRestart(t *testing.T) {
	f := &fakeMDNS{}
	m := newHostForMDNS(func() (mdnsService, error) { _ = f.Start(); return f, nil })
	if err := m.restartMDNS(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	m.closeMDNS()
	if _, closed := f.counts(); closed != 1 {
		t.Errorf("closeMDNS should close the active service; closed=%d", closed)
	}
	if err := m.restartMDNS(); err != nil {
		t.Fatalf("restart after close should be a no-op, got %v", err)
	}
	if m.mdnsHealthy() {
		t.Fatal("restart after close must not revive the service")
	}
}

// TestSuperviseMDNSRecoversDeadService verifies the supervisor loop restarts a
// service that has silently died (simulated by nil-ing the active service),
// within one (shortened) refresh interval — the reliability guarantee.
func TestSuperviseMDNSRecoversDeadService(t *testing.T) {
	var built int32
	m := newHostForMDNS(func() (mdnsService, error) {
		atomic.AddInt32(&built, 1)
		f := &fakeMDNS{}
		_ = f.Start()
		return f, nil
	})
	m.mdnsRefresh = 5 * time.Millisecond
	if err := m.restartMDNS(); err != nil { // initial start (build #1)
		t.Fatalf("initial: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.superviseMDNS(ctx)

	// Simulate a silent death: drop the active service so mdnsHealthy is false.
	m.mdnsMu.Lock()
	m.mdns = nil
	m.mdnsMu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.mdnsHealthy() && atomic.LoadInt32(&built) >= 2 {
			return // supervisor rebuilt it
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("supervisor did not recover the dead service; builds=%d healthy=%v",
		atomic.LoadInt32(&built), m.mdnsHealthy())
}
