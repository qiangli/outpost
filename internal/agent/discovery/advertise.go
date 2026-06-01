// mDNS advertisement: register ourselves on `_outpost._tcp.local`.
//
// The service-instance name is the AssignedHostname when present
// (cloudbox-issued slug), falling back to a sanitized AgentName.
// Either way it must be DNS-safe: lowercase letters, digits, and
// hyphens. The .local resolution that OSes already do for free
// gives any other host on the LAN a working `<name>.local` →
// IP lookup with no extra protocol on the caller's side.
//
// TXT records carry the Tier-1 metadata. The full set comes from
// AdvertiseOptions:
//
//	id   = PeerID (SHA256 fingerprint of the host key)
//	an   = AgentName
//	host = AssignedHostname (when different from `an`)
//	user = OS username the outpost runs as
//	email= operator OAuth2 email (when paired; "" otherwise)
//	cb   = cloudbox base URL (when paired; "" otherwise)
//	ver  = build commit
//	pair = "1" when paired, "0" otherwise
//	ssh  = SSH LAN listener `host:port` (when bound)
//	http = HTTP discover LAN listener `host:port` (when bound)
//
// Receivers ignore unknown keys, so this set is forward-compatible.
package discovery

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/mdns"
)

// ServiceName is the DNS-SD service type we advertise under and
// browse on. Matches RFC 6763 §7 conventions.
const ServiceName = "_outpost._tcp"

// AdvertiseOptions parameterises one mDNS service registration.
// All string fields are passed through to TXT records as-is (the
// caller is responsible for sanitization).
type AdvertiseOptions struct {
	// InstanceName is the service-instance label. Becomes
	// `<InstanceName>._outpost._tcp.local`. Must be a single DNS
	// label (no dots).
	InstanceName string

	// Port is the primary LAN port advertised in the SRV record.
	// Pick whichever endpoint is most useful as the "default" dial
	// target — the LAN SSH listener if bound, else the HTTP
	// discover listener, else the admin/MCP listener.
	Port int

	// IPs are the addresses to advertise. When empty,
	// hashicorp/mdns auto-detects from local interfaces.
	IPs []string

	// PeerID, AgentName, AssignedHostname, OSUsername, OAuth2Email,
	// CloudboxBase, Version, Paired, SSHListenAddr,
	// HTTPDiscoverListenAddr feed the TXT records.
	PeerID                 PeerID
	AgentName              string
	AssignedHostname       string
	OSUsername             string
	OAuth2Email            string
	CloudboxBase           string
	Version                string
	Paired                 bool
	SSHListenAddr          string
	HTTPDiscoverListenAddr string
}

// Advertiser owns one running mDNS registration. Close stops the
// goroutines and removes the service from the LAN.
type Advertiser struct {
	server *mdns.Server
}

// Advertise starts an mDNS server announcing this outpost. The
// returned Advertiser must be closed; the supplied context cancellation
// is also honored — when ctx is done, the registration is removed.
func Advertise(ctx context.Context, opts AdvertiseOptions) (*Advertiser, error) {
	if opts.InstanceName == "" {
		return nil, fmt.Errorf("discovery: empty InstanceName")
	}
	if opts.Port <= 0 {
		return nil, fmt.Errorf("discovery: invalid Port %d", opts.Port)
	}
	service, err := mdns.NewMDNSService(
		opts.InstanceName,
		ServiceName,
		"", // domain — defaults to .local
		"", // hostName — defaults to OS hostname
		opts.Port,
		nil, // ips — let the library auto-detect
		buildTXTRecords(opts),
	)
	if err != nil {
		return nil, fmt.Errorf("discovery: build mdns service: %w", err)
	}

	srv, err := mdns.NewServer(&mdns.Config{Zone: service})
	if err != nil {
		return nil, fmt.Errorf("discovery: start mdns server: %w", err)
	}

	// Wire ctx cancellation to Shutdown so callers can plumb a
	// single context through their lifecycle and rely on it.
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown()
	}()

	return &Advertiser{server: srv}, nil
}

// Close stops the mDNS server and removes our service registration
// from the LAN. Safe to call after the context-driven Shutdown.
func (a *Advertiser) Close() error {
	if a == nil || a.server == nil {
		return nil
	}
	return a.server.Shutdown()
}

// buildTXTRecords serializes the Tier-1 metadata into a slice of
// `<key>=<value>` TXT entries. Empty values are omitted to keep the
// records short. RFC 6763 §6 recommends total TXT size under 1300
// bytes (well-tested headers); we stay well under that.
func buildTXTRecords(opts AdvertiseOptions) []string {
	entries := []struct {
		k, v string
	}{
		{"id", string(opts.PeerID)},
		{"an", opts.AgentName},
		{"host", opts.AssignedHostname},
		{"user", opts.OSUsername},
		{"email", opts.OAuth2Email},
		{"cb", opts.CloudboxBase},
		{"ver", opts.Version},
		{"pair", paired01(opts.Paired)},
		{"ssh", opts.SSHListenAddr},
		{"http", opts.HTTPDiscoverListenAddr},
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		v := strings.TrimSpace(e.v)
		if v == "" {
			continue
		}
		out = append(out, e.k+"="+v)
	}
	return out
}

func paired01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
