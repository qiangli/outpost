// Package shard orchestrates a Prima.cpp pipelined-ring shard over the mesh
// forwarder: serving a model bigger than any single node by splitting its layers
// across paired mesh peers. Each rank binds its zmq data+signal ports; the mesh
// forwarder carries the ring (rank→next) and the master-spoke (every rank→rank0)
// over hole-punched, encrypted peer links, so no rank touches the network
// directly — the firewall / ssh-proxy / elevation pain of the raw cross-machine
// proof disappears. cloudbox brokers the introduction; bytes go peer-to-peer.
//
// Prima topology (verified from prima.cpp src/llama.cpp, map_rank_to_port =
// base+rank):
//   - each rank BINDS    data(DataPort+rank) and signal(SignalPort+rank)
//   - each rank CONNECTS  to next-rank data+signal (the ring) and rank0 data
//     (the master-spoke; rank0 dials itself = loopback, skipped)
//
// This replaces the earlier llama.cpp-RPC *star* skeleton — Prima.cpp (proven
// cross-machine 2026-06-29) is the engine. See docs/distributed-inference-v0-plan.md
// and docs/distributed-inference-sota-map.md.
package shard

import (
	"fmt"
	"net"
	"sort"
	"strconv"
)

// Prima.cpp port bases (src/llama.cpp). DataPort is the `--data-port` default;
// SignalPort is fixed in prima (no CLI flag). Rank i uses base+i for each.
const (
	DefaultDataPort   = 9000
	DefaultSignalPort = 10000

	// Mesh-forwarder service names this host's ring channels are exposed under.
	DataService   = "shard-data"
	SignalService = "shard-signal"

	loopback = "127.0.0.1"
)

// Forwarder is the subset of *mesh.Forwarder the shard needs. The daemon passes
// the real forwarder (which satisfies this); the interface keeps the wiring
// unit-testable without the whole mesh surface.
type Forwarder interface {
	Expose(service, loopbackAddr string)
	Unexpose(service string)
	Listen(localAddr, peerID, service string) (net.Listener, error)
}

// Member is one rank in the pipelined ring. Rank 0 is the leader (master +
// prompt driver). PeerID is the member's libp2p peer id; empty marks THIS host.
type Member struct {
	Rank   int    `json:"rank"`
	Host   string `json:"host"`    // label for logging
	PeerID string `json:"peer_id"` // libp2p peer id; "" = this host
}

// Ring is a full shard placement: one Member per rank (any order) plus the port
// bases. Zero ports default to the prima defaults.
type Ring struct {
	Members    []Member `json:"members"`
	DataPort   int      `json:"data_port,omitempty"`   // 0 → DefaultDataPort
	SignalPort int      `json:"signal_port,omitempty"` // 0 → DefaultSignalPort
}

// Expose is a loopback service this host publishes over the mesh (its bound
// prima port).
type Expose struct {
	Service string `json:"service"`
	Addr    string `json:"addr"` // 127.0.0.1:(base+myRank)
}

// Forward is a local listener this host opens that bridges to a peer's exposed
// service — bound at the EXACT loopback port prima will dial.
type Forward struct {
	LocalAddr string `json:"local_addr"` // 127.0.0.1:(port prima connects to)
	PeerID    string `json:"peer_id"`
	Service   string `json:"service"`
}

// HostPlan is the computed mesh wiring + prima distributed args for one host.
type HostPlan struct {
	MyRank   int       `json:"my_rank"`
	World    int       `json:"world"`
	DataPort int       `json:"data_port"`
	Exposes  []Expose  `json:"exposes"`
	Forwards []Forward `json:"forwards"`
	// PrimaArgs is the distributed-inference flag set. The launcher appends the
	// model (`-m`), and for rank 0 the prompt (`-p`/`-n`); workers add none.
	PrimaArgs []string `json:"prima_args"`
}

// normalize sorts members by rank, validates a contiguous 0..N-1 set with unique
// ranks, and fills default ports.
func (r Ring) normalize() (members []Member, dataPort, signalPort int, err error) {
	n := len(r.Members)
	if n < 2 {
		return nil, 0, 0, fmt.Errorf("shard: ring needs >=2 members, got %d", n)
	}
	members = append(members, r.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].Rank < members[j].Rank })
	for i, m := range members {
		if m.Rank != i {
			return nil, 0, 0, fmt.Errorf("shard: ranks must be contiguous 0..%d; got rank %d at position %d", n-1, m.Rank, i)
		}
	}
	dataPort, signalPort = r.DataPort, r.SignalPort
	if dataPort == 0 {
		dataPort = DefaultDataPort
	}
	if signalPort == 0 {
		signalPort = DefaultSignalPort
	}
	return members, dataPort, signalPort, nil
}

// PlanFor computes the mesh wiring + prima args for the host running rank myRank.
// Pure — no I/O — so the ring logic is fully unit-testable.
func (r Ring) PlanFor(myRank int) (*HostPlan, error) {
	members, dataPort, signalPort, err := r.normalize()
	if err != nil {
		return nil, err
	}
	n := len(members)
	if myRank < 0 || myRank >= n {
		return nil, fmt.Errorf("shard: myRank %d out of range 0..%d", myRank, n-1)
	}
	next := (myRank + 1) % n

	addrOf := func(port int) string { return loopback + ":" + strconv.Itoa(port) }

	// This host publishes its own data+signal ports under the well-known names.
	exposes := []Expose{
		{Service: DataService, Addr: addrOf(dataPort + myRank)},
		{Service: SignalService, Addr: addrOf(signalPort + myRank)},
	}

	// Forward targets (next rank, and rank 0 for the master-spoke) are remote, so
	// they must carry a peer id.
	if members[next].PeerID == "" {
		return nil, fmt.Errorf("shard: next member (rank %d, host %q) has no peer id", next, members[next].Host)
	}
	if myRank != 0 && members[0].PeerID == "" {
		return nil, fmt.Errorf("shard: master (rank 0, host %q) has no peer id", members[0].Host)
	}

	// Every outbound prima connection becomes a local forward at the exact port
	// prima dials. Dedupe by local addr (the last rank's next==0 makes the
	// ring-data and master-spoke forwards coincide).
	fwd := map[string]Forward{}
	add := func(port int, peerID, service string) {
		addr := addrOf(port)
		fwd[addr] = Forward{LocalAddr: addr, PeerID: peerID, Service: service}
	}
	add(dataPort+next, members[next].PeerID, DataService)     // ring data → next
	add(signalPort+next, members[next].PeerID, SignalService) // ring signal → next
	if myRank != 0 {                                          // master-spoke; rank 0 dials itself (loopback)
		add(dataPort+0, members[0].PeerID, DataService)
	}

	forwards := make([]Forward, 0, len(fwd))
	for _, f := range fwd {
		forwards = append(forwards, f)
	}
	sort.Slice(forwards, func(i, j int) bool { return forwards[i].LocalAddr < forwards[j].LocalAddr })

	primaArgs := []string{
		"--world", strconv.Itoa(n),
		"--rank", strconv.Itoa(myRank),
		"--master", loopback,
		"--next", loopback,
		"--data-port", strconv.Itoa(dataPort),
	}

	return &HostPlan{
		MyRank:    myRank,
		World:     n,
		DataPort:  dataPort,
		Exposes:   exposes,
		Forwards:  forwards,
		PrimaArgs: primaArgs,
	}, nil
}

// FullArgs builds the complete prima argv: the distributed flags, the model, any
// caller extras (e.g. --prefetch, --gpu-mem, and for rank 0 the -p/-n prompt).
func (p *HostPlan) FullArgs(modelPath string, extra ...string) []string {
	args := append([]string{}, p.PrimaArgs...)
	args = append(args, "-m", modelPath)
	args = append(args, extra...)
	return args
}

// Apply wires this host's plan into the mesh forwarder: Expose every local
// service, then open every forward listener. Fail-closed — any Listen error
// unwinds everything already opened, so a half-formed ring never exists. The
// returned cleanup closes the listeners and unexposes the services.
func Apply(f Forwarder, p *HostPlan) (cleanup func(), err error) {
	for _, e := range p.Exposes {
		f.Expose(e.Service, e.Addr)
	}
	unexposeAll := func() {
		for _, e := range p.Exposes {
			f.Unexpose(e.Service)
		}
	}

	var lns []net.Listener
	for _, fw := range p.Forwards {
		ln, lerr := f.Listen(fw.LocalAddr, fw.PeerID, fw.Service)
		if lerr != nil {
			closeAll(lns)
			unexposeAll()
			return nil, fmt.Errorf("shard: forward %s → (%s, %s): %w", fw.LocalAddr, fw.PeerID, fw.Service, lerr)
		}
		lns = append(lns, ln)
	}

	return func() {
		closeAll(lns)
		unexposeAll()
	}, nil
}

func closeAll(lns []net.Listener) {
	for _, l := range lns {
		_ = l.Close()
	}
}
