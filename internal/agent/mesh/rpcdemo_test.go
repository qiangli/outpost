//go:build meshrpcdemo

package mesh

import (
	"os"
	"testing"
	"time"
)

// TestRPCForwardDemo is a manual, build-tagged demo (never runs in normal CI):
// it stands two in-process mesh hosts, exposes a real rpc-server's loopback on
// the worker, and forwards a local listener to it on the client — then holds
// the forward open so a real llama-server can be pointed at the listener.
//
//	# worker rpc-server on 127.0.0.1:50053, leader points llama-server at LISTEN_ADDR
//	RPC_ADDR=127.0.0.1:50053 LISTEN_ADDR=127.0.0.1:5555 HOLD=120 \
//	  go test -tags meshrpcdemo -run TestRPCForwardDemo -timeout 5m ./internal/agent/mesh/
func TestRPCForwardDemo(t *testing.T) {
	rpcAddr := os.Getenv("RPC_ADDR")
	listenAddr := os.Getenv("LISTEN_ADDR")
	if rpcAddr == "" || listenAddr == "" {
		t.Skip("set RPC_ADDR (worker rpc-server loopback) + LISTEN_ADDR (leader local)")
	}
	hold := 120 * time.Second
	if h := os.Getenv("HOLD"); h != "" {
		if d, err := time.ParseDuration(h + "s"); err == nil {
			hold = d
		}
	}

	worker := newTestHost(t)
	defer worker.Close()
	client := newTestHost(t)
	defer client.Close()
	connectHosts(t, client, worker)

	worker.Forwarder().Expose("rpc", rpcAddr)
	ln, err := client.Forwarder().Listen(listenAddr, worker.PeerID(), "rpc")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	t.Logf("MESH FORWARD UP: %s  --mesh-->  %s (worker peer %s)", ln.Addr(), rpcAddr, worker.PeerID())
	t.Logf("point: llama-server --rpc %s", ln.Addr())
	time.Sleep(hold)
}
