package mesh

import (
	"context"
	"log/slog"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	mdns "github.com/libp2p/go-libp2p/p2p/discovery/mdns"
)

// mdnsServiceTag is a CUSTOM mDNS service name so only our outposts discover
// each other on the LAN — not arbitrary libp2p apps that happen to share the
// segment. (libp2p's default would be "_p2p._udp".)
const mdnsServiceTag = "dhnt-outpost-mesh"

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
	// host.Connect dials the discovered on-link addrs → a direct LAN link.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
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
