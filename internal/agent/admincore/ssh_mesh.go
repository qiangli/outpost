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

// meshSSHDialTimeout bounds the WS dial over the local mesh forward.
const meshSSHDialTimeout = 10 * time.Second

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
	peers, err := s.MeshResolve(meshSSHService)
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

	ticket, err := peerticket.Exchange(ctx, s.deps.CloudboxBase, bearer, cookie, host, "ssh")
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
