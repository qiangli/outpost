package admincore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMeshForward records Listen/CloseListen so a test can tell whether the
// mesh rung opened (and released) a forward.
type fakeMeshForward struct {
	listens, closes atomic.Int32
	addr            string
}

func (f *fakeMeshForward) Expose(string, string) error { return nil }
func (f *fakeMeshForward) Unexpose(string) error       { return nil }
func (f *fakeMeshForward) Listen(peerID, service, localAddr string) (string, error) {
	f.listens.Add(1)
	return f.addr, nil
}
func (f *fakeMeshForward) CloseListen(string) error  { f.closes.Add(1); return nil }
func (f *fakeMeshForward) Forwards() MeshForwardView { return MeshForwardView{} }

// meshTestServer builds a Server whose mesh holds a link to "peer-host" with
// peer id "p1" (direct or not), whose registry lists the given ssh publishers,
// and whose cloudbox counts peer-ticket requests.
func meshTestServer(t *testing.T, direct bool, publishers []MeshResolvedPeer) (*Server, *fakeMeshForward, *atomic.Int32) {
	t.Helper()
	var tickets atomic.Int32
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ssh/peer-ticket" {
			tickets.Add(1)
			_, _ = w.Write([]byte(`{"ticket":"t1"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(cb.Close)
	// The forward points at a closed port so the WS dial fails fast: the
	// test stops at "the mesh rung applied", not a full ssh session.
	fwd := &fakeMeshForward{addr: "127.0.0.1:1"}
	s := &Server{deps: Deps{
		CloudboxBase: cb.URL,
		MeshForward:  fwd,
		MeshPeerIDByHost: func(host string) string {
			if host == "peer-host" {
				return "p1"
			}
			return ""
		},
		MeshStatus: func() *MeshStatusView {
			return &MeshStatusView{Peers: []MeshPeerConnView{{ID: "p1", Direct: direct, LinkClass: "lan"}}}
		},
		MeshResolver: func(service string) ([]MeshResolvedPeer, error) {
			if service != meshSSHService {
				t.Errorf("resolved service %q, want %q", service, meshSSHService)
			}
			return publishers, nil
		},
	}}
	return s, fwd, &tickets
}

func TestMeshSSHTransportNoDirectLinkFallsBack(t *testing.T) {
	s, fwd, tickets := meshTestServer(t, false, []MeshResolvedPeer{{Host: "peer-host", PeerID: "p1"}})
	_, _, err := s.meshSSHTransport(context.Background(), "bearer", "peer-host", "cookie")
	if !errors.Is(err, errMeshSSHNotAvailable) {
		t.Fatalf("relayed (non-direct) link: err = %v, want errMeshSSHNotAvailable", err)
	}
	if fwd.listens.Load() != 0 || tickets.Load() != 0 {
		t.Fatalf("non-direct link opened %d forwards and %d tickets, want 0/0", fwd.listens.Load(), tickets.Load())
	}
}

func TestMeshSSHTransportUnknownHostFallsBack(t *testing.T) {
	s, fwd, _ := meshTestServer(t, true, nil)
	if _, _, err := s.meshSSHTransport(context.Background(), "bearer", "other-host", "cookie"); !errors.Is(err, errMeshSSHNotAvailable) {
		t.Fatalf("host with no mesh peer: err = %v, want errMeshSSHNotAvailable", err)
	}
	if fwd.listens.Load() != 0 {
		t.Fatal("opened a forward for a host the mesh does not know")
	}
}

func TestMeshSSHTransportUnpublishedPeerSkipsForwardAndTicket(t *testing.T) {
	s, fwd, tickets := meshTestServer(t, true, []MeshResolvedPeer{{Host: "someone-else", PeerID: "p9"}})
	if _, _, err := s.meshSSHTransport(context.Background(), "bearer", "peer-host", "cookie"); !errors.Is(err, errMeshSSHNotAvailable) {
		t.Fatalf("peer not publishing ssh: err = %v, want errMeshSSHNotAvailable", err)
	}
	if fwd.listens.Load() != 0 || tickets.Load() != 0 {
		t.Fatalf("unpublished peer opened %d forwards and %d tickets, want 0/0", fwd.listens.Load(), tickets.Load())
	}
}

func TestMeshSSHTransportNoCookieFallsBack(t *testing.T) {
	s, fwd, _ := meshTestServer(t, true, []MeshResolvedPeer{{Host: "peer-host", PeerID: "p1"}})
	if _, _, err := s.meshSSHTransport(context.Background(), "bearer", "peer-host", ""); !errors.Is(err, errMeshSSHNotAvailable) {
		t.Fatalf("no elevation cookie: err = %v, want errMeshSSHNotAvailable", err)
	}
	if fwd.listens.Load() != 0 {
		t.Fatal("opened a forward without an elevation cookie to trade for a ticket")
	}
}

// A direct link to a publishing peer is the mesh path: it opens a forward and
// mints a ticket. The dial to the fake forward fails, which must surface as a
// real error (logged, then relay fallback) and release the forward.
func TestMeshSSHTransportDirectPublishedPeerTakesMeshAndReleasesOnFailure(t *testing.T) {
	s, fwd, tickets := meshTestServer(t, true, []MeshResolvedPeer{{Host: "peer-host", PeerID: "p1"}})
	_, _, err := s.meshSSHTransport(context.Background(), "bearer", "peer-host", "cookie")
	if err == nil || errors.Is(err, errMeshSSHNotAvailable) {
		t.Fatalf("direct publishing peer with a dead forward: err = %v, want a real dial error", err)
	}
	if fwd.listens.Load() != 1 || tickets.Load() != 1 {
		t.Fatalf("mesh path opened %d forwards and %d tickets, want 1/1", fwd.listens.Load(), tickets.Load())
	}
	if fwd.closes.Load() != 1 {
		t.Fatalf("failed mesh dial closed %d forwards, want 1 (no leaked listener)", fwd.closes.Load())
	}
}

// A registry lookup that hangs (slow or degraded cloudbox) must not hold up
// the relay fallback: the rung bows out within its resolve budget.
func TestMeshSSHTransportSlowResolveBowsOutWithinBudget(t *testing.T) {
	s, fwd, tickets := meshTestServer(t, true, nil)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s.deps.MeshResolver = func(string) ([]MeshResolvedPeer, error) {
		<-release
		return nil, nil
	}
	start := time.Now()
	_, _, err := s.meshSSHTransport(context.Background(), "bearer", "peer-host", "cookie")
	if !errors.Is(err, errMeshSSHNotAvailable) {
		t.Fatalf("hung resolve: err = %v, want errMeshSSHNotAvailable", err)
	}
	if took := time.Since(start); took > meshSSHResolveTimeout+time.Second {
		t.Fatalf("hung resolve held the rung for %s, budget %s", took, meshSSHResolveTimeout)
	}
	if fwd.listens.Load() != 0 || tickets.Load() != 0 {
		t.Fatalf("hung resolve opened %d forwards and %d tickets, want 0/0", fwd.listens.Load(), tickets.Load())
	}
}

// The full rung (transport + handshake) must return a real error — never a
// half-open client — when the mesh leg fails, so dialSSHChain falls back to
// the relay; the forward is released.
func TestMeshSSHClientFailureReleasesForwardForRelayFallback(t *testing.T) {
	s, fwd, _ := meshTestServer(t, true, []MeshResolvedPeer{{Host: "peer-host", PeerID: "p1"}})
	cli, cleanup, err := s.meshSSHClient(context.Background(), "bearer", "peer-host", "user", "cookie", t.TempDir()+"/known_hosts")
	if err == nil || cli != nil || cleanup != nil {
		t.Fatalf("failed mesh leg returned cli=%v cleanup=%v err=%v; want nil, nil, error", cli, cleanup != nil, err)
	}
	if fwd.closes.Load() != 1 {
		t.Fatalf("failed mesh leg closed %d forwards, want 1", fwd.closes.Load())
	}
}
