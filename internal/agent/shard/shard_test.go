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

// TestShardForwardsReachWorkerRPC proves the orchestration: the leader's --rpc
// forward address reaches the worker's exposed rpc-server over the mesh (here a
// stand-in echo server in place of a real rpc-server).
func TestShardForwardsReachWorkerRPC(t *testing.T) {
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

	// worker: a stand-in rpc-server (echo) exposed as the shard rpc service.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); _, _ = io.Copy(c, c) }(c)
		}
	}()
	ExposeLocalRPC(worker.Forwarder(), echoLn.Addr().String())

	// leader: open the forward(s) for the worker(s).
	addrs, lns, err := LeaderForwards(leader.Forwarder(), []Worker{
		{Host: "worker", PeerID: worker.PeerID()},
	})
	if err != nil {
		t.Fatalf("LeaderForwards: %v", err)
	}
	defer closeAll(lns)
	if len(addrs) != 1 {
		t.Fatalf("expected 1 rpc addr, got %d", len(addrs))
	}

	// the --rpc address reaches the worker's rpc-server over the mesh.
	conn, err := net.Dial("tcp", addrs[0])
	if err != nil {
		t.Fatalf("dial rpc addr: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	msg := []byte("shard-rpc-over-mesh")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("mismatch: %q != %q", got, msg)
	}
}

// A worker with no peer id is rejected — never a half-formed shard.
func TestLeaderForwardsRejectsMissingPeerID(t *testing.T) {
	leader := newMeshHost(t)
	defer leader.Close()
	if _, _, err := LeaderForwards(leader.Forwarder(), []Worker{{Host: "w"}}); err == nil {
		t.Fatal("expected error for a worker with no peer id")
	}
}
