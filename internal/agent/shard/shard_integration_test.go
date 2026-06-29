package shard

import (
	"context"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/qiangli/outpost/internal/agent/mesh"
)

func newMeshHost(t *testing.T) *mesh.Host {
	t.Helper()
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h, err := mesh.New(mesh.Config{ListenPort: 0, PrivKey: priv})
	if err != nil {
		t.Fatalf("mesh host: %v", err)
	}
	return h
}

// TestShardChannelsOverRealMesh proves the shard's Apply wiring carries bytes
// end-to-end over the REAL mesh forwarder (not the fake): the worker Apply()s
// its host plan against the production *mesh.Forwarder — exposing its prima data
// port as DataService — and the leader opens the forward the ring uses to reach
// rank 1's data port and gets through to the worker's (stand-in) prima recv
// socket over the hole-punchable peer link.
//
// It exercises the worker side of Apply for real. The leader's forward is opened
// directly rather than via its own Apply because both full plans share fixed
// ports that COLLIDE in one process (the leader's forward to rank 1's data port
// == the worker's bind of that same port) — the genuine both-sides run is the
// post-deploy cross-machine smoke (docs/distributed-inference-v0-plan.md, v0c),
// where 127.0.0.1:PORT on each machine is distinct.
func TestShardChannelsOverRealMesh(t *testing.T) {
	worker := newMeshHost(t)
	defer worker.Close()
	leader := newMeshHost(t)
	defer leader.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := leader.LibP2PHost().Connect(ctx, peer.AddrInfo{
		ID:    worker.LibP2PHost().ID(),
		Addrs: worker.LibP2PHost().Addrs(),
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Stand-in for the worker's prima recv socket, at rank 1's data port (9001).
	echo, err := net.Listen("tcp", "127.0.0.1:9001")
	if err != nil {
		t.Skipf("rank-1 data port 9001 busy, skipping: %v", err)
	}
	defer echo.Close()
	go func() {
		for {
			c, aerr := echo.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); _, _ = io.Copy(c, c) }(c)
		}
	}()

	// Worker side: Apply the rank-1 plan against the REAL forwarder. rank 0 (the
	// leader) carries the leader's peer id so the worker's master/signal forwards
	// are well-formed (they're unused here — we assert only the leader→worker
	// data hop).
	ring := Ring{Members: []Member{
		{Rank: 0, Host: "leader", PeerID: leader.PeerID()},
		{Rank: 1, Host: "worker", PeerID: worker.PeerID()},
	}}
	plan, err := ring.PlanFor(1)
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := Apply(worker.Forwarder(), plan)
	if err != nil {
		t.Fatalf("worker Apply: %v", err)
	}
	defer cleanup()

	// Leader side: the forward the ring uses to reach rank 1's data service.
	ln, err := leader.Forwarder().Listen("127.0.0.1:0", worker.PeerID(), DataService)
	if err != nil {
		t.Fatalf("leader Listen: %v", err)
	}
	defer ln.Close()

	// Bytes flow: leader's loopback → mesh → worker's exposed shard-data (echo).
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial leader forward: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	msg := []byte("shard-data-over-mesh")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("echo mismatch over mesh: %q != %q", got, msg)
	}
}
