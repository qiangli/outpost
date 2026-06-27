package mesh

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

func connectHosts(t *testing.T, from, to *Host) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := from.LibP2PHost().Connect(ctx, peer.AddrInfo{
		ID:    to.LibP2PHost().ID(),
		Addrs: to.LibP2PHost().Addrs(),
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
}

// TestForwardOverMesh proves the full transport: a TCP client → the client-side
// forward listener → a mesh stream → the worker-side handler → the worker's
// local echo service, with the echo round-tripping back.
func TestForwardOverMesh(t *testing.T) {
	worker := newTestHost(t)
	defer worker.Close()
	client := newTestHost(t)
	defer client.Close()
	connectHosts(t, client, worker)

	// worker: a local echo TCP server, exposed over the mesh as "echo".
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
	worker.Forwarder().Expose("echo", echoLn.Addr().String())

	// client: a forward listener → (worker, "echo").
	fwdLn, err := client.Forwarder().Listen("127.0.0.1:0", worker.PeerID(), "echo")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer fwdLn.Close()

	conn, err := net.Dial("tcp", fwdLn.Addr().String())
	if err != nil {
		t.Fatalf("dial forward listener: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	msg := []byte("mesh-forward-roundtrip")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("echo mismatch: got %q want %q", got, msg)
	}
}

// An un-exposed service is refused — the allowlist is the security boundary, so
// a peer can't reach an arbitrary local port. The worker resets the stream,
// which closes the client's bridged connection.
func TestForwardRejectsUnknownService(t *testing.T) {
	worker := newTestHost(t)
	defer worker.Close()
	client := newTestHost(t)
	defer client.Close()
	connectHosts(t, client, worker)
	// worker exposes nothing.

	fwdLn, err := client.Forwarder().Listen("127.0.0.1:0", worker.PeerID(), "nope")
	if err != nil {
		t.Fatal(err)
	}
	defer fwdLn.Close()

	conn, err := net.Dial("tcp", fwdLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	_, _ = conn.Write([]byte("x"))

	if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
		t.Fatal("expected the connection to be closed for an un-exposed service")
	}
}
