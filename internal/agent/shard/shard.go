// Package shard is the Ollama sharding orchestration — serving a model bigger
// than any single node by splitting it across mesh peers via llama.cpp RPC,
// carried over the mesh forwarder (sprint #8) rather than a fragile raw TCP
// socket. See docs/ollama-sharding-builtin-plan.md + docs/libp2p-mesh-transport.md.
//
// The transport is the now-proven mesh forwarder: each worker Expose()s its
// local rpc-server loopback over the mesh; the leader opens one local forward
// Listen() per worker and points `llama-server --rpc <addr1>,<addr2>,…` at the
// returned loopback addresses. cloudbox brokers the introduction; the RPC bytes
// go peer-to-peer. (Actual serving additionally needs a working llama.cpp rpc
// build — the upstream b9817 get_tensor crash is orthogonal to this transport.)
package shard

import (
	"fmt"
	"net"
)

// RPCService is the mesh-forwarder service name shard-RPC is exposed under.
const RPCService = "rpc"

// Forwarder is the subset of the mesh forwarder the shard needs. The daemon
// passes the real *mesh.Forwarder (which satisfies this); keeping it an
// interface lets the orchestration be tested without the whole mesh surface.
type Forwarder interface {
	Expose(service, loopbackAddr string)
	Listen(localAddr, peerID, service string) (net.Listener, error)
}

// Worker is one shard worker — a paired mesh peer running an rpc-server.
type Worker struct {
	Host   string // peer hostname (for logging/labels)
	PeerID string // libp2p peer id
}

// ExposeLocalRPC is the WORKER side: expose this host's local rpc-server
// loopback (e.g. "127.0.0.1:50052") over the mesh so the leader can reach it.
func ExposeLocalRPC(f Forwarder, rpcLoopbackAddr string) {
	f.Expose(RPCService, rpcLoopbackAddr)
}

// LeaderForwards is the LEADER side: open one local forward listener per worker
// (each bridging over the mesh to that worker's exposed rpc-server) and return
// the local addresses to pass to `llama-server --rpc`, alongside the listeners
// (close them to tear the shard down). On any failure it closes the listeners
// it already opened and returns the error — never a half-formed shard.
func LeaderForwards(f Forwarder, workers []Worker) (rpcAddrs []string, listeners []net.Listener, err error) {
	for _, w := range workers {
		if w.PeerID == "" {
			closeAll(listeners)
			return nil, nil, fmt.Errorf("shard: worker %q has no peer id", w.Host)
		}
		ln, lerr := f.Listen("127.0.0.1:0", w.PeerID, RPCService)
		if lerr != nil {
			closeAll(listeners)
			return nil, nil, fmt.Errorf("shard: forward to worker %s: %w", w.Host, lerr)
		}
		rpcAddrs = append(rpcAddrs, ln.Addr().String())
		listeners = append(listeners, ln)
	}
	return rpcAddrs, listeners, nil
}

func closeAll(lns []net.Listener) {
	for _, l := range lns {
		_ = l.Close()
	}
}
