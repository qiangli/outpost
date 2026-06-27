package mesh

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	ma "github.com/multiformats/go-multiaddr"
)

// TestRelayCircuit proves a peer reaches another THROUGH a cloudbox-style relay:
// the relay runs the circuit-v2 relay service, B reserves a slot, and A dials
// B's circuit address — the connection forms. This is the introduction two
// strict-NAT peers need before DCUtR can upgrade the relayed link to a direct
// one. (Here all hosts are on loopback, so the point proven is the relay path,
// not NAT traversal itself.)
func TestRelayCircuit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The relay — what cloudbox runs.
	relay, err := libp2p.New(
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"),
		libp2p.EnableRelayService(),
		libp2p.ForceReachabilityPublic(), // a relay advertises hop only when public
	)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	defer relay.Close()
	relayInfo := peer.AddrInfo{ID: relay.ID(), Addrs: relay.Addrs()}

	a := newTestHost(t)
	defer a.Close()
	b := newTestHost(t)
	defer b.Close()

	if err := a.LibP2PHost().Connect(ctx, relayInfo); err != nil {
		t.Fatalf("a->relay: %v", err)
	}
	if err := b.LibP2PHost().Connect(ctx, relayInfo); err != nil {
		t.Fatalf("b->relay: %v", err)
	}

	// B reserves a relay slot so peers can reach it via the circuit.
	if _, err := client.Reserve(ctx, b.LibP2PHost(), relayInfo); err != nil {
		t.Fatalf("b reserve: %v", err)
	}

	// A dials B through the relay circuit.
	circuit, err := ma.NewMultiaddr("/p2p/" + relay.ID().String() + "/p2p-circuit/p2p/" + b.PeerID())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.LibP2PHost().Connect(ctx, peer.AddrInfo{
		ID:    b.LibP2PHost().ID(),
		Addrs: []ma.Multiaddr{circuit},
	}); err != nil {
		t.Fatalf("a->b via relay: %v", err)
	}

	// Connected via the relay. A relay-only link reports Limited; if DCUtR has
	// already upgraded it to a direct link it reports Connected. Either proves
	// the relay brokered the introduction.
	c := a.LibP2PHost().Network().Connectedness(b.LibP2PHost().ID())
	if c != network.Connected && c != network.Limited {
		t.Fatalf("a should be connected to b via the relay, got %v", c)
	}
}

// A host configured with a relay constructs cleanly + runs AutoRelay.
func TestHostWithRelayConfig(t *testing.T) {
	relay, err := libp2p.New(
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"),
		libp2p.EnableRelayService(),
		libp2p.ForceReachabilityPublic(), // a relay advertises hop only when public
	)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	defer relay.Close()

	p2pAddrs, err := peer.AddrInfoToP2pAddrs(&peer.AddrInfo{ID: relay.ID(), Addrs: relay.Addrs()})
	if err != nil || len(p2pAddrs) == 0 {
		t.Fatalf("relay p2p addrs: %v", err)
	}
	h := newTestHostWithRelay(t, p2pAddrs[0].String())
	defer h.Close()
	// Give AutoRelay a moment to attempt a reservation; the host stays healthy.
	time.Sleep(time.Second)
	if h.PeerID() == "" {
		t.Fatal("host should have a peer id")
	}
}
