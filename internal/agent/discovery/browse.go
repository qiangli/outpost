// mDNS browse: one-shot query for `_outpost._tcp.local` returning the
// parsed Peer records. Watch (continuous) is a thin loop on top of
// Browse for the daemon's discovery cache.
package discovery

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/netip"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
)

// quietMDNSLogger discards hashicorp/mdns's internal log output. The
// library's default logger dumps the whole client struct on Close
// ("[INFO] mdns: Closing client {true true 0x…}") and logs per-instance
// query failures — noise for an operator running `outpost scan`. The
// meaningful error is returned from mdns.Query and handled by the
// caller, so dropping the library's chatter loses nothing.
var quietMDNSLogger = log.New(io.Discard, "", 0)

// BrowseOptions configures one query.
type BrowseOptions struct {
	// Timeout caps the query duration. The mDNS responder window
	// is intentionally short (1–2s is plenty on a working LAN).
	// Default 3s.
	Timeout time.Duration

	// SelfPeerID, when non-empty, filters out entries matching the
	// caller's own PeerID. The mDNS responder library doesn't
	// suppress one's own announcements; we do it here.
	SelfPeerID PeerID
}

// Browse queries the LAN for outpost service instances and returns
// the parsed Peer records. Blocks up to Timeout.
//
// It first queries dual-stack (IPv4 + IPv6). hashicorp/mdns aborts the
// whole query if either multicast send fails, and IPv6 mDNS to ff02::fb
// fails with "no route to host" on any host lacking an IPv6 multicast
// route (common on macOS and IPv4-only LANs) — even though the IPv4
// query (224.0.0.251, the workhorse) already went out fine. So on a
// send failure we fall back to an IPv4-only query rather than surfacing
// the IPv6 error. IPv6 is kept when it works.
func Browse(ctx context.Context, opts BrowseOptions) ([]Peer, error) {
	if opts.Timeout == 0 {
		opts.Timeout = 3 * time.Second
	}

	peers, err := browseOnce(ctx, opts, false)
	if err != nil && ctx.Err() == nil {
		// Dual-stack send failed before we could listen (zero entries).
		// Retry IPv4-only — degrades gracefully on hosts where IPv6
		// multicast is unroutable. If IPv4 is the broken transport,
		// this second attempt returns the IPv4 error to the caller.
		if v4, v4err := browseOnce(ctx, opts, true); v4err == nil {
			return v4, nil
		}
	}
	return peers, err
}

// browseOnce runs a single mDNS query. disableIPv6 forces an IPv4-only
// query (the fallback path).
func browseOnce(ctx context.Context, opts BrowseOptions, disableIPv6 bool) ([]Peer, error) {
	// hashicorp/mdns uses a channel + a Query goroutine; queue size
	// 64 is generous for our LAN scale (rarely > 10 outposts at once).
	entriesCh := make(chan *mdns.ServiceEntry, 64)
	queryDone := make(chan error, 1)

	go func() {
		params := &mdns.QueryParam{
			Service:     ServiceName,
			Domain:      "local",
			Timeout:     opts.Timeout,
			Entries:     entriesCh,
			DisableIPv6: disableIPv6,
			Logger:      quietMDNSLogger,
		}
		err := mdns.Query(params)
		// Closing the channel signals the receiver that no more
		// entries are coming.
		close(entriesCh)
		queryDone <- err
	}()

	peers := make([]Peer, 0, 8)
	seen := make(map[PeerID]struct{})

	for {
		select {
		case <-ctx.Done():
			return peers, ctx.Err()
		case entry, ok := <-entriesCh:
			if !ok {
				// channel closed; query goroutine done. Drain
				// queryDone before returning so we don't leak it.
				select {
				case err := <-queryDone:
					if err != nil {
						return peers, fmt.Errorf("mdns query: %w", err)
					}
				default:
				}
				return peers, nil
			}
			peer, ok := serviceEntryToPeer(entry)
			if !ok {
				continue
			}
			if peer.ID == opts.SelfPeerID && peer.ID != "" {
				continue
			}
			if _, dup := seen[peer.ID]; dup && peer.ID != "" {
				continue
			}
			if peer.ID != "" {
				seen[peer.ID] = struct{}{}
			}
			peers = append(peers, peer)
		}
	}
}

// serviceEntryToPeer parses an mdns.ServiceEntry into a discovery.Peer.
// Returns (_, false) when the entry doesn't look like an outpost
// announcement (no `id=` TXT — our marker).
func serviceEntryToPeer(e *mdns.ServiceEntry) (Peer, bool) {
	if e == nil {
		return Peer{}, false
	}
	txt := parseTXT(e.InfoFields)
	id, hasID := txt["id"]
	if !hasID || id == "" {
		return Peer{}, false
	}

	p := Peer{
		ID:               PeerID(id),
		AgentName:        txt["an"],
		AssignedHostname: txt["host"],
		OSUsername:       txt["user"],
		OAuth2Email:      txt["email"],
		Version:          txt["ver"],
		CloudboxBase:     txt["cb"],
		Paired:           txt["pair"] == "1",
		Sources:          []Source{SourceMDNS},
		Trust:            TrustUnverified,
		LastSeenAt:       time.Now(),
	}

	// Collect addresses from the entry. mdns.ServiceEntry exposes
	// AddrV4 / AddrV6; both may be set or just one.
	if e.AddrV4 != nil {
		if addr, ok := netip.AddrFromSlice(e.AddrV4.To4()); ok {
			p.Addrs = append(p.Addrs, addr)
		}
	}
	if e.AddrV6 != nil {
		if addr, ok := netip.AddrFromSlice(e.AddrV6.To16()); ok {
			p.Addrs = append(p.Addrs, addr)
		}
	}

	// Build Endpoints from the TXT-advertised listener addresses.
	// SSH and HTTP-discover are LAN listeners advertised by the
	// outpost itself; the cloudbox endpoint is added by callers
	// (we don't know the cloudbox-side host:port shape from TXT).
	addEndpointFromAddr(&p, EndpointLANSSH, txt["ssh"])
	addEndpointFromAddr(&p, EndpointLANSSHWS, txt["sshws"])
	addEndpointFromAddr(&p, EndpointLANHTTPDiscover, txt["http"])

	// Fall back to the SRV record's port when no TXT listener
	// fields were present (older outposts, or operators advertising
	// without LAN listeners). The host comes from the first
	// resolved address.
	if len(p.Endpoints) == 0 && e.Port > 0 && len(p.Addrs) > 0 {
		p.Endpoints = append(p.Endpoints, Endpoint{
			Kind: EndpointLANHTTPDiscover, // best guess; caller can recheck
			Host: p.Addrs[0].String(),
			Port: e.Port,
		})
	}

	return p, true
}

// addEndpointFromAddr parses a `host:port` (or just `:port`) listen
// spec and appends it as an Endpoint. The host falls back to the
// `<assigned_hostname>.local` form when only a port is present, so
// LAN .local resolution carries the dial forward.
func addEndpointFromAddr(p *Peer, kind EndpointKind, listenSpec string) {
	listenSpec = strings.TrimSpace(listenSpec)
	if listenSpec == "" {
		return
	}
	host, port := splitHostPortLoose(listenSpec)
	if port <= 0 {
		return
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		// Bind-to-all. Use the assigned hostname (`.local` resolves
		// it) or the first resolved IP.
		if p.AssignedHostname != "" {
			host = p.AssignedHostname + ".local"
		} else if len(p.Addrs) > 0 {
			host = p.Addrs[0].String()
		}
	}
	if host == "" {
		return
	}
	p.Endpoints = append(p.Endpoints, Endpoint{
		Kind: kind,
		Host: host,
		Port: port,
	})
}

// splitHostPortLoose accepts `host:port`, `:port`, or `port`. Returns
// (host, port). Port is 0 on parse failure (caller treats as "skip").
func splitHostPortLoose(s string) (string, int) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", 0
	}
	// Strip an optional `tcp://` or scheme:// prefix.
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	host := ""
	portStr := s
	if i := strings.LastIndex(s, ":"); i >= 0 {
		host = s[:i]
		portStr = s[i+1:]
	}
	port := 0
	for _, r := range portStr {
		if r < '0' || r > '9' {
			return host, 0
		}
		port = port*10 + int(r-'0')
		if port > 65535 {
			return host, 0
		}
	}
	return host, port
}

// parseTXT collapses a slice of `key=value` records into a map. Keys
// without an `=` are stored with empty values; later occurrences of
// the same key overwrite earlier ones.
func parseTXT(records []string) map[string]string {
	out := make(map[string]string, len(records))
	for _, r := range records {
		k, v, ok := strings.Cut(r, "=")
		if !ok {
			out[r] = ""
			continue
		}
		out[k] = v
	}
	return out
}
