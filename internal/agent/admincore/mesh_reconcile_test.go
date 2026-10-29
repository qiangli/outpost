package admincore

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// flakyMeshFwd is a MeshForwardOps fake whose Listen fails the first
// failTimes calls (the boot-time bind race: the WSL vEthernet address is not
// up yet, or the map entry died after a peer reconnect) and whose listeners
// can be killed out from under the server to simulate an accept-loop death.
type flakyMeshFwd struct {
	mu        sync.Mutex
	failTimes int
	listens   int
	listeners map[string]MeshListenerView
}

func (f *flakyMeshFwd) Expose(service, addr string) error { return nil }
func (f *flakyMeshFwd) Unexpose(service string) error     { return nil }

func (f *flakyMeshFwd) Listen(peerID, service, localAddr string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listens++
	if f.listens <= f.failTimes {
		return "", errors.New("forward: bind: cannot assign requested address")
	}
	if f.listeners == nil {
		f.listeners = map[string]MeshListenerView{}
	}
	addr := localAddr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	f.listeners[addr] = MeshListenerView{Addr: addr, PeerID: peerID, Service: service}
	return addr, nil
}

func (f *flakyMeshFwd) CloseListen(addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.listeners, addr)
	return nil
}

func (f *flakyMeshFwd) Forwards() MeshForwardView {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := MeshForwardView{Exposed: map[string]string{}}
	for _, l := range f.listeners {
		v.Listeners = append(v.Listeners, l)
	}
	return v
}

// kill drops a listener without going through CloseListen — the forwarder's
// accept goroutine dying on its own (it removes itself from the map).
func (f *flakyMeshFwd) kill(addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.listeners, addr)
}

func newConsumeTestServer(t *testing.T, fwd MeshForwardOps) *Server {
	t.Helper()
	s, err := New(Deps{
		ConfigPath:  filepath.Join(t.TempDir(), "agent.json"),
		MeshForward: fwd,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A boot-time bind failure must not lose the persisted consume: it stays
// listed as not-live, and a later reconcile binds it (retry until bound).
func TestMeshConsumeReconcileRetriesLostBind(t *testing.T) {
	fwd := &flakyMeshFwd{failTimes: 1}
	s := newConsumeTestServer(t, fwd)

	// Upsert persists even though the live bind fails (boot race).
	if _, err := s.MeshConsumeUpsert("llm-door", "peerA", "127.0.0.1:49940"); err == nil {
		t.Fatal("expected the first bind to fail")
	}

	cons, err := s.MeshConsumes()
	if err != nil {
		t.Fatal(err)
	}
	if len(cons) != 1 {
		t.Fatalf("persisted consumes=%+v", cons)
	}
	if cons[0].Live {
		t.Fatalf("consume must report not-live while the bind fails: %+v", cons[0])
	}

	// Retry: the next reconcile binds it and it reports live.
	if err := s.ReconcileMeshConsumes(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cons, _ = s.MeshConsumes(); len(cons) != 1 || !cons[0].Live {
		t.Fatalf("consume must report live after reconcile: %+v", cons)
	}
}

// A listener that dies after a peer reconnect must read not-live and be
// re-bound by the next reconcile.
func TestMeshConsumeReconcileRebindsDeadListener(t *testing.T) {
	fwd := &flakyMeshFwd{}
	s := newConsumeTestServer(t, fwd)

	if _, err := s.MeshConsumeUpsert("llm-door", "peerA", "127.0.0.1:49940"); err != nil {
		t.Fatal(err)
	}
	if cons, _ := s.MeshConsumes(); !cons[0].Live {
		t.Fatalf("consume must start live: %+v", cons)
	}

	fwd.kill("127.0.0.1:49940")
	if cons, _ := s.MeshConsumes(); cons[0].Live {
		t.Fatalf("dead listener must report not-live: %+v", cons[0])
	}

	if err := s.ReconcileMeshConsumes(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cons, _ := s.MeshConsumes(); !cons[0].Live {
		t.Fatalf("reconcile must re-bind the dead listener: %+v", cons)
	}
	if got := fwd.listens; got != 2 {
		t.Fatalf("expected exactly one re-bind, got %d Listens", got)
	}
}

// The background reconciler performs an immediate pass and returns when its
// context ends.
func TestMeshConsumeReconcilerRunsAndStops(t *testing.T) {
	fwd := &flakyMeshFwd{failTimes: 1}
	s := newConsumeTestServer(t, fwd)

	if _, err := s.MeshConsumeUpsert("llm-door", "peerA", "127.0.0.1:49940"); err == nil {
		t.Fatal("expected the first bind to fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RunMeshConsumeReconciler(ctx, 5*time.Millisecond)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		cons, _ := s.MeshConsumes()
		if len(cons) == 1 && cons[0].Live {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("reconciler never bound the consume: %+v", cons)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reconciler did not stop on context cancel")
	}
}

// A reconcile that still cannot bind must keep reporting the failure (so the
// retry loop backs off and tries again) rather than failing silently.
func TestMeshConsumeReconcileSurfacesPersistentFailure(t *testing.T) {
	fwd := &flakyMeshFwd{failTimes: 100}
	s := newConsumeTestServer(t, fwd)

	if _, err := s.MeshConsumeUpsert("llm-door", "peerA", "127.0.0.1:49940"); err == nil {
		t.Fatal("expected the bind to fail")
	}
	if err := s.ReconcileMeshConsumes(); err == nil {
		t.Fatal("expected reconcile to surface the bind failure")
	}
	if cons, _ := s.MeshConsumes(); cons[0].Live {
		t.Fatalf("consume must stay not-live while binds fail: %+v", cons[0])
	}
}
