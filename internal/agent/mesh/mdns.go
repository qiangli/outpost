package mesh

import (
	"context"
	"log/slog"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	mdns "github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
)

// mdnsServiceTag is a CUSTOM mDNS service name so only our outposts discover
// each other on the LAN — not arbitrary libp2p apps that happen to share the
// segment. (libp2p's default would be "_p2p._udp".)
const mdnsServiceTag = "dhnt-outpost-mesh"

// mdnsService is the subset of mdns.Service the Host lifecycle needs — an
// interface so the supervisor's restart logic is unit-testable behind a fake,
// without real multicast. (*mdns.Service satisfies it.)
type mdnsService interface {
	Start() error
	Close() error
}

// mdnsNotifee reacts to peers discovered via local-network mDNS multicast by
// dialing their on-link addresses, forming a DIRECT LAN connection. This is the
// complement to the cloudbox rendezvous, which only hands over WAN-oriented
// addresses: two boxes on the same LAN would otherwise hole-punch over the
// public IP (relayed/WAN) instead of meeting on their on-link address.
type mdnsNotifee struct {
	h   host.Host
	log *slog.Logger
}

// HandlePeerFound is invoked by the mDNS resolver for every advertised peer it
// sees on the local network (including, occasionally, ourselves).
func (n *mdnsNotifee) HandlePeerFound(pi peer.AddrInfo) {
	if pi.ID == n.h.ID() {
		return // self — our own multicast advertisement
	}
	if n.h.Network().Connectedness(pi.ID) == network.Connected {
		return // already connected (direct or via the rendezvous path)
	}
	// The peer is advertised on the LAN but NOT currently connected. This covers
	// the reliability case the daemon-restart bug exposed: a peer that dropped
	// and came back with NEW ephemeral addrs, or one whose prior dial is parked
	// in libp2p's dial backoff so a plain Connect (which trusts stale peerstore
	// addrs) never retries. Refresh the peerstore with the freshly-discovered
	// on-link addrs, clear any dial backoff, and force a DIRECT dial.
	n.h.Peerstore().AddAddrs(pi.ID, pi.Addrs, peerstore.TempAddrTTL)
	if sw, ok := n.h.Network().(*swarm.Swarm); ok {
		sw.Backoff().Clear(pi.ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = network.WithForceDirectDial(ctx, "mesh-mdns-rediscover")
	if err := n.h.Connect(ctx, pi); err != nil {
		n.log.Debug("mesh mdns: LAN peer dial failed", "peer", pi.ID, "err", err)
		return
	}
	n.log.Info("mesh mdns: direct LAN peer connected", "peer", pi.ID, "addrs", pi.Addrs)
}

// startMDNS starts local-network mDNS peer discovery for the given host. The
// returned service multicasts the host's on-link addresses and resolves sibling
// outposts on the same LAN; close it to stop advertising/resolving. A failure to
// start is non-fatal at the call site — the host degrades to the relay/
// rendezvous path.
func startMDNS(h host.Host, log *slog.Logger) (mdns.Service, error) {
	if log == nil {
		log = slog.Default()
	}
	svc := mdns.NewMdnsService(h, mdnsServiceTag, &mdnsNotifee{h: h, log: log})
	return svc, svc.Start()
}
