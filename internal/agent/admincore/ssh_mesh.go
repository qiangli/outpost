package admincore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/peerticket"
	"github.com/qiangli/yoke/pkg/sshclient"
)

// meshSSHService is the mesh service name under which an outpost publishes
// its sshws endpoint (cmd/outpost MeshServiceSSH).
const meshSSHService = "ssh"

// Per-step budgets for the mesh rung. They keep a degraded control plane
// (slow registry or ticket endpoint) or a stalled peer from delaying the
// relay fallback: the rung gives up within seconds and the exec proceeds
// through the tunnel as it did before the rung existed.
const (
	meshSSHResolveTimeout   = 2 * time.Second
	meshSSHTicketTimeout    = 4 * time.Second
	meshSSHDialTimeout      = 10 * time.Second
	meshSSHHandshakeTimeout = 15 * time.Second
)

// errMeshSSHNotAvailable means mesh-direct does not apply to this host (mesh
// off, no DIRECT link, the peer publishes no ssh, or cloudbox has no ticket
// endpoint). The caller falls back to the cloudbox tunnel without a warning.
var errMeshSSHNotAvailable = errors.New("mesh-direct not available")

// meshSSHTransport opens the outer ssh transport to host over an EXISTING
// direct mesh link, so a same-LAN peer is reached peer-to-peer instead of
// through the cloudbox relay. cloudbox still authorizes the connection (the
// single-use peer ticket); only the bytes skip the relay.
//
// It returns the transport and a cleanup that closes the local mesh forward.
// errMeshSSHNotAvailable means the path does not apply; any other error means
// it applied and failed. Both fall back to the tunnel.
//
// Only a DIRECT link qualifies: a relayed libp2p circuit is still a hop
// through someone else's infrastructure and must not displace the tunnel.
func (s *Server) meshSSHTransport(ctx context.Context, bearer, host, cookie string) (net.Conn, func(), error) {
	if s.deps.CloudboxBase == "" || s.deps.MeshForward == nil || strings.TrimSpace(cookie) == "" {
		return nil, nil, errMeshSSHNotAvailable
	}
	link := s.MeshHostLink(host)
	if !link.Found || !link.Direct || link.PeerID == "" {
		return nil, nil, errMeshSSHNotAvailable
	}
	// Confirm the target publishes ssh before opening a forward: Listen binds
	// eagerly and connects lazily, so a peer that serves nothing would only
	// fail after a wasted forward and ticket round-trip.
	peers, err := s.meshResolveWithin(meshSSHService, meshSSHResolveTimeout)
	if err != nil {
		return nil, nil, errMeshSSHNotAvailable
	}
	publishes := false
	for _, p := range peers {
		if p.PeerID == link.PeerID || strings.EqualFold(p.Host, host) {
			publishes = true
			break
		}
	}
	if !publishes {
		return nil, nil, errMeshSSHNotAvailable
	}
	addr, err := s.MeshListen(link.PeerID, meshSSHService, "127.0.0.1:0")
	if err != nil || addr == "" {
		return nil, nil, errMeshSSHNotAvailable
	}
	closeForward := func() { _ = s.MeshCloseListen(addr) }

	ticketCtx, ticketCancel := context.WithTimeout(ctx, meshSSHTicketTimeout)
	ticket, err := peerticket.Exchange(ticketCtx, s.deps.CloudboxBase, bearer, cookie, host, "ssh")
	ticketCancel()
	if err != nil {
		closeForward()
		if errors.Is(err, peerticket.ErrNotDeployed) {
			return nil, nil, errMeshSSHNotAvailable
		}
		return nil, nil, fmt.Errorf("peer-ticket exchange: %w", err)
	}
	wsConn, err := sshclient.DialWS(ctx, sshclient.DialOptions{
		WSURL:       "ws://" + addr + "/ssh",
		PeerTicket:  ticket,
		Host:        host,
		DialTimeout: meshSSHDialTimeout,
	})
	if err != nil {
		closeForward()
		return nil, nil, fmt.Errorf("mesh-direct dial %s: %w", host, err)
	}
	return sshclient.AsNetConn(ctx, wsConn), closeForward, nil
}

// meshSSHClient is the whole mesh rung: transport AND ssh handshake. Doing the
// handshake here (not in the caller's shared code) is what lets a mesh leg
// that fails late — a stream that dies, a peer that rejects the ticket — fall
// back to the relay instead of failing the exec. The returned cleanup closes
// the local forward; the caller owns the client.
func (s *Server) meshSSHClient(ctx context.Context, bearer, host, user, cookie, knownHostsPath string) (*sshclient.Client, func(), error) {
	conn, closeForward, err := s.meshSSHTransport(ctx, bearer, host, cookie)
	if err != nil {
		return nil, nil, err
	}
	hostKeyCB, err := sshclient.KnownHostsCallbackTOFU(knownHostsPath, sshclient.HostAliasForHost(host))
	if err != nil {
		_ = conn.Close()
		closeForward()
		return nil, nil, fmt.Errorf("known_hosts callback: %w", err)
	}
	cli, err := sshclient.Dial(ctx, sshclient.Config{
		Transport:        conn,
		HostAlias:        sshclient.HostAliasForHost(host),
		User:             user,
		HostKeyCallback:  hostKeyCB,
		HandshakeTimeout: meshSSHHandshakeTimeout,
	})
	if err != nil {
		closeForward()
		return nil, nil, fmt.Errorf("ssh handshake to %s (mesh-direct): %w", host, err)
	}
	return cli, closeForward, nil
}

// meshResolveWithin bounds the registry lookup (a cloudbox call that takes no
// context) so a slow control plane cannot hold up the relay fallback. On
// timeout the lookup finishes in the background and its result is dropped.
func (s *Server) meshResolveWithin(service string, d time.Duration) ([]MeshResolvedPeer, error) {
	type result struct {
		peers []MeshResolvedPeer
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		peers, err := s.MeshResolve(service)
		ch <- result{peers, err}
	}()
	select {
	case r := <-ch:
		return r.peers, r.err
	case <-time.After(d):
		return nil, fmt.Errorf("mesh resolve %q: timed out after %s", service, d)
	}
}
