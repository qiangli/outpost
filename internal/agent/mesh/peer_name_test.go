package mesh

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/host/peerstore/pstoremem"
)

// The neighbour name comes from the peer's own announced user-agent, so a
// LAN neighbour list needs no cloudbox (sprint 220, story ed857a89).
func TestPeerNameFromAgent(t *testing.T) {
	ps, err := pstoremem.NewPeerstore()
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	pid, err := peer.Decode("12D3KooWRRq3Lxgb4rtb3VyptQj9RaaQT41h5VjioVpNs3BNdgUs")
	if err != nil {
		t.Fatal(err)
	}
	if got := peerNameFromAgent(ps, pid); got != "" {
		t.Fatalf("unknown peer named %q", got)
	}
	_ = ps.Put(pid, "AgentVersion", "outpost-mesh/winbox")
	if got := peerNameFromAgent(ps, pid); got != "winbox" {
		t.Fatalf("name = %q, want winbox", got)
	}
	_ = ps.Put(pid, "AgentVersion", "go-libp2p/0.41")
	if got := peerNameFromAgent(ps, pid); got != "" {
		t.Fatalf("a non-outpost peer got a name: %q", got)
	}
}

// Two live hosts: after connecting, each sees the other's name in its
// mesh status — the wire really carries it.
func TestStatusNamesAConnectedPeer(t *testing.T) {
	h1 := newTestHostNamed(t, "alpha")
	h2 := newTestHostNamed(t, "beta")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ai := peer.AddrInfo{ID: h2.LibP2PHost().ID(), Addrs: h2.LibP2PHost().Addrs()}
	if err := h1.LibP2PHost().Connect(ctx, ai); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range h1.Status().Peers {
			if p.Name == "beta" {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("h1 never named h2: %+v", h1.Status().Peers)
}

func newTestHostNamed(t *testing.T, name string) *Host {
	t.Helper()
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{ListenPort: 0, PrivKey: priv, DisableMDNS: true, AgentName: name})
	if err != nil {
		t.Fatalf("new host: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}
