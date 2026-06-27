package mesh

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// ForwardProtocol is the libp2p stream protocol for the generic loopback-TCP
// forwarder. A client opens a stream, writes the target service name, and the
// remote host bridges the stream to that service's local loopback address.
const ForwardProtocol = "/dhnt/mesh/forward/1.0.0"

// Forwarder carries a local loopback TCP service over the mesh — the transport
// the rest of the fabric rides on. Two halves:
//   - EXPOSER (worker): registers allowlisted local services; a stream handler
//     bridges each inbound stream to the named service's loopback address.
//   - DIALER (client/leader): opens a local TCP listener that bridges every
//     accepted connection over a fresh mesh stream to a (peer, service).
//
// This is the transport under shard-RPC (a loopback rpc-server Expose()d here,
// the leader's llama-server pointed at a local Listen() address) and
// peer-backup. Only allowlisted services are reachable — a connected peer can
// never dial an arbitrary local port, which is what makes exposing a loopback
// service over the mesh safe.
type Forwarder struct {
	host *Host
	log  *slog.Logger

	mu        sync.RWMutex
	exposed   map[string]string         // service name → loopback addr (e.g. "rpc" → "127.0.0.1:50052")
	listeners map[string]*listenerEntry // bound addr → active forward listener
}

type listenerEntry struct {
	ln      net.Listener
	peerID  string
	service string
}

func newForwarder(host *Host, log *slog.Logger) *Forwarder {
	if log == nil {
		log = slog.Default()
	}
	f := &Forwarder{
		host:      host,
		log:       log,
		exposed:   map[string]string{},
		listeners: map[string]*listenerEntry{},
	}
	host.h.SetStreamHandler(ForwardProtocol, f.handleStream)
	return f
}

// ForwardSnapshot is the live state of this host's forwarder.
type ForwardSnapshot struct {
	Exposed   map[string]string `json:"exposed"`   // service → loopback addr
	Listeners []ForwardListener `json:"listeners"` // active forward listeners
}

// ForwardListener describes one active forward listener.
type ForwardListener struct {
	Addr    string `json:"addr"`
	PeerID  string `json:"peer_id"`
	Service string `json:"service"`
}

// Snapshot returns the forwarder's exposed services + active listeners.
func (f *Forwarder) Snapshot() ForwardSnapshot {
	f.mu.RLock()
	defer f.mu.RUnlock()
	exp := make(map[string]string, len(f.exposed))
	for k, v := range f.exposed {
		exp[k] = v
	}
	lis := make([]ForwardListener, 0, len(f.listeners))
	for addr, e := range f.listeners {
		lis = append(lis, ForwardListener{Addr: addr, PeerID: e.peerID, Service: e.service})
	}
	return ForwardSnapshot{Exposed: exp, Listeners: lis}
}

// CloseListen closes the forward listener bound at addr.
func (f *Forwarder) CloseListen(addr string) error {
	f.mu.Lock()
	e := f.listeners[addr]
	f.mu.Unlock()
	if e == nil {
		return fmt.Errorf("forward: no listener at %s", addr)
	}
	return e.ln.Close() // the accept goroutine removes it from the map
}

// Expose registers a local loopback service reachable over the mesh under name
// (e.g. Expose("rpc", "127.0.0.1:50052")). Only exposed services are reachable;
// re-exposing a name replaces its address.
func (f *Forwarder) Expose(name, loopbackAddr string) {
	f.mu.Lock()
	f.exposed[name] = loopbackAddr
	f.mu.Unlock()
}

// Unexpose removes a service from the allowlist.
func (f *Forwarder) Unexpose(name string) {
	f.mu.Lock()
	delete(f.exposed, name)
	f.mu.Unlock()
}

// Listen opens a local TCP listener; every accepted connection is bridged over
// a fresh mesh stream to (peerID, service) on the remote host. Close the
// returned listener to stop forwarding. localAddr "" → 127.0.0.1:0 (ephemeral).
func (f *Forwarder) Listen(localAddr, peerID, service string) (net.Listener, error) {
	pid, err := peer.Decode(peerID)
	if err != nil {
		return nil, fmt.Errorf("forward: bad peer id: %w", err)
	}
	if localAddr == "" {
		localAddr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", localAddr)
	if err != nil {
		return nil, err
	}
	addr := ln.Addr().String()
	f.mu.Lock()
	f.listeners[addr] = &listenerEntry{ln: ln, peerID: peerID, service: service}
	f.mu.Unlock()
	go func() {
		defer func() {
			f.mu.Lock()
			delete(f.listeners, addr)
			f.mu.Unlock()
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go f.dialAndBridge(conn, pid, service)
		}
	}()
	f.log.Info("mesh forward: listening", "addr", addr, "peer", peerID, "service", service)
	return ln, nil
}

func (f *Forwarder) dialAndBridge(conn net.Conn, pid peer.ID, service string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := f.host.h.NewStream(ctx, pid, ForwardProtocol)
	if err != nil {
		f.log.Debug("mesh forward: open stream failed", "peer", pid.String(), "err", err)
		conn.Close()
		return
	}
	if err := writeService(s, service); err != nil {
		s.Reset()
		conn.Close()
		return
	}
	bridge(s, conn)
}

// handleStream bridges an inbound forward stream to its (allowlisted) local
// service. Unknown services are reset — a peer can't reach arbitrary ports.
func (f *Forwarder) handleStream(s network.Stream) {
	_ = s.SetReadDeadline(time.Now().Add(10 * time.Second))
	name, err := readService(s)
	if err != nil {
		s.Reset()
		return
	}
	_ = s.SetReadDeadline(time.Time{})

	f.mu.RLock()
	addr, ok := f.exposed[name]
	f.mu.RUnlock()
	if !ok {
		f.log.Debug("mesh forward: unknown service", "service", name, "peer", s.Conn().RemotePeer().String())
		s.Reset()
		return
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		f.log.Debug("mesh forward: dial local failed", "addr", addr, "err", err)
		s.Reset()
		return
	}
	bridge(s, conn)
}

// bridge copies bytes both ways between a mesh stream and a TCP conn, half-
// closing each direction on EOF, then fully closing both.
func bridge(s network.Stream, conn net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(s, conn)
		_ = s.CloseWrite()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, s)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	_ = s.Close()
	_ = conn.Close()
}

// writeService frames a service name as a single length byte + the name.
func writeService(w io.Writer, name string) error {
	if len(name) == 0 || len(name) > 255 {
		return fmt.Errorf("forward: service name must be 1..255 bytes")
	}
	if _, err := w.Write([]byte{byte(len(name))}); err != nil {
		return err
	}
	_, err := io.WriteString(w, name)
	return err
}

func readService(r io.Reader) (string, error) {
	var l [1]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return "", err
	}
	n := int(l[0])
	if n == 0 {
		return "", fmt.Errorf("forward: empty service name")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}
