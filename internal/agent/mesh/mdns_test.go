package mesh

import (
	"io"
	"log/slog"
	"testing"

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
