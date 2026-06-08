// Package discovery exposes LAN peer-finding (mDNS browse + HTTP /discover)
// plus the data structures shared across advertisers, browsers, the HTTP
// surface, and the discovery cache.
//
// Identity model (Wave 3A.1, TOFU; Wave 3A.2 lifts to cloudbox-signed
// certs):
//
//   - PeerID = SHA256 fingerprint of the outpost's ed25519 host key. Same
//     value `ssh.FingerprintSHA256(pubkey)` returns. Stable across the
//     lifetime of the host key (never rotated except by re-pair).
//   - Hostname identity = AgentName (operator-chosen) + AssignedHostname
//     (cloudbox-assigned slug; equals os.Hostname() in 3A.1 before the
//     cloudbox change lands).
//   - Resource-owner identity = OAuth2Email — Tier-2 trust anchor.
//   - OS user = OSUsername — informational + future SSH user-cert flow.
//
// Trust tiers, repeated from the plan so reviewers don't have to context-
// switch:
//
//   - Tier 1 (open, no cert needed): `/discover/hello`, mDNS, NAT hints,
//     `outpost scan`, `outpost://peers`. Just metadata.
//   - Tier 2 (cert-or-TOFU verified): `/discover/probe`, `/peers`,
//     `/gossip`, plus everything that *acts on* a peer (ssh exec, jump,
//     sftp, repair binary).
package discovery

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// PeerID is the canonical peer identity. Format matches
// ssh.FingerprintSHA256 output, e.g.
//
//	"SHA256:Z6JEnskW1k5N2OFEcLmRpY+UDc/yX4tFr8r5KH8e0Dk"
//
// We use this rather than the bare base64 because the prefix is
// self-describing (an operator looking at scan output knows what kind
// of identifier they're seeing) and it round-trips with ssh-keygen / SSH
// known_hosts output.
type PeerID string

// IsValid reports whether the PeerID has the SHA256 fingerprint shape.
// Cheap syntactic check; does not verify the underlying key.
func (p PeerID) IsValid() bool {
	s := string(p)
	if !strings.HasPrefix(s, "SHA256:") {
		return false
	}
	return len(s) > len("SHA256:") && len(s) < 128
}

// EndpointKind names the transport + role of an Endpoint. The set is
// open-ended (a future Wave can add quic, webrtc, etc.) but Wave 3A.1
// only emits the four below.
type EndpointKind string

const (
	// EndpointLANSSH is the outpost's optional LAN TCP SSH listener
	// (FileConfig.SSHListenAddr). Reachable directly from peers on
	// the same broadcast domain. PAM-gated (no cloudbox-vouching on
	// LAN-direct) — the legacy plain-TCP path before peer-tickets.
	EndpointLANSSH EndpointKind = "lan-ssh"

	// EndpointLANSSHWS is the outpost's optional LAN WebSocket-mounted
	// SSH listener (FileConfig.SSHWSListenAddr). Speaks the same
	// /ssh route the loopback handler does, plus accepts peer-ticket
	// JWTs as the auth signal (replacing the cloudbox-stamped
	// X-Periscope-Role header that's only trustworthy on loopback).
	// Lets `outpost ssh <peer>` stay passwordless on the LAN-direct
	// path without putting cloudbox on the data plane.
	EndpointLANSSHWS EndpointKind = "lan-ssh-ws"

	// EndpointLANHTTPDiscover is the optional LAN HTTP discovery
	// listener (FileConfig.DiscoveryHTTPListenAddr). Hosts the
	// `/api/v1/discover/*` surface.
	EndpointLANHTTPDiscover EndpointKind = "lan-http-discover"

	// EndpointLANAdminMCP is the admin/MCP listener
	// (FileConfig.AdminAddr) when bound to a LAN address. Most
	// operators leave this on loopback; we report it as a LAN
	// endpoint only when it's actually LAN-reachable.
	EndpointLANAdminMCP EndpointKind = "lan-admin-mcp"

	// EndpointCloudboxSSH is the cloudbox-fronted /h/<host>/ssh
	// path. Always present for a paired outpost. Used as fallback
	// reachability when the peer is not on our LAN.
	EndpointCloudboxSSH EndpointKind = "cloudbox-ssh"
)

// Endpoint is one reachable address for a Peer. Multiaddr-shape
// borrowed from libp2p: the Kind names the transport so future
// transports plug in without reshaping the struct.
type Endpoint struct {
	Kind EndpointKind `json:"kind"`
	Host string       `json:"host"` // IP literal, DNS name, or assigned_hostname.local
	Port int          `json:"port"` // 0 when not applicable (e.g. cloudbox WS path)
}

// HostPort returns the conventional dialing form. Hides the
// zero-port case behind an obvious-when-broken result.
func (e Endpoint) HostPort() string {
	if e.Port == 0 {
		return e.Host
	}
	return fmt.Sprintf("%s:%d", e.Host, e.Port)
}

// Source records how the local cache first heard about a Peer. The
// list is additive — when the same peer is rediscovered via multiple
// channels, we union the Sources rather than picking one.
type Source string

const (
	SourceMDNS         Source = "mdns"
	SourceHTTPProbe    Source = "http-probe"
	SourceCloudboxHint Source = "cloudbox-nat-hint"
	SourceGossip       Source = "gossip"
	SourceHistory      Source = "history"  // we connected to them before; remembering from disk
	SourceOperator     Source = "operator" // explicit `outpost discover probe <url>`
)

// TrustLevel summarises what we know about the peer's identity.
// Wave 3A.1 only emits TOFU and Unverified; Wave 3A.2 adds CloudboxCert
// once cloudbox-side CA work lands.
type TrustLevel string

const (
	TrustUnverified   TrustLevel = "unverified"    // metadata only; no cryptographic check has been done
	TrustTOFU         TrustLevel = "tofu"          // fingerprint pinned to known_hosts (first-contact)
	TrustCloudboxCert TrustLevel = "cloudbox-cert" // cloudbox-CA-signed cert verified (Wave 3A.2)
)

// Peer is one discovered outpost. Endpoints are ordered by
// preference (LAN-direct first, cloudbox fallback last). Sources
// records how we found out about it. LastSeenAt is updated on every
// fresh observation across any source.
type Peer struct {
	ID PeerID `json:"id"`

	AgentName        string `json:"agent_name"`
	AssignedHostname string `json:"assigned_hostname,omitempty"`

	// Tier-2 identity facts. Populated when the peer presented a
	// cert (Wave 3A.2) or when we already know them via cloudbox.
	OAuth2Email string `json:"oauth2_email,omitempty"`
	OSUsername  string `json:"os_username,omitempty"`

	Endpoints    []Endpoint   `json:"endpoints"`
	Addrs        []netip.Addr `json:"-"` // resolved IPs from mDNS; not serialized in MCP/JSON (use Endpoints)
	Version      string       `json:"version,omitempty"`
	CloudboxBase string       `json:"cloudbox_base,omitempty"`
	Paired       bool         `json:"paired"`

	Sources    []Source   `json:"sources"`
	Trust      TrustLevel `json:"trust"`
	LastSeenAt time.Time  `json:"last_seen_at"`

	// Active marks a peer as part of the HyParView "active view" —
	// the bounded set of peers we maintain hot connections to and
	// preferentially gossip with. Roadmap item #16. Default false
	// (= passive view); promotion happens in Cache.markActiveLocked
	// when there's headroom (activeMax) and the peer has freshly
	// been observed via a transport we trust.
	Active bool `json:"active,omitempty"`
}

// HasEndpoint reports whether the peer advertises a reachable
// endpoint of the given kind. Used by the dial path to decide
// whether to attempt a LAN-direct dial vs falling back to cloudbox.
func (p *Peer) HasEndpoint(k EndpointKind) bool {
	for _, e := range p.Endpoints {
		if e.Kind == k {
			return true
		}
	}
	return false
}

// FirstEndpoint returns the first endpoint of the given kind, or a
// zero Endpoint when none matches. Callers compare against the zero
// to distinguish "no endpoint" from "endpoint with empty host."
func (p *Peer) FirstEndpoint(k EndpointKind) Endpoint {
	for _, e := range p.Endpoints {
		if e.Kind == k {
			return e
		}
	}
	return Endpoint{}
}

// AddSource is the canonical "union" operation when we re-observe a
// peer via a new channel. Idempotent.
func (p *Peer) AddSource(s Source) {
	if slices.Contains(p.Sources, s) {
		return
	}
	p.Sources = append(p.Sources, s)
}

// ReachabilityEdge is one observation from the reachability ledger:
// "I (Self) successfully reached Peer via Endpoint at Time."
// Used by Component 6 (edge gossip, Wave 3B) and Component 7
// (temporal observations).
type ReachabilityEdge struct {
	Self PeerID `json:"self"`
	// Peer is the destination's fingerprint when we have one (set
	// for cert-verified or TOFU-pinned peers). For Wave 3B.1 cloudbox-
	// reached hosts dialed by alias we don't yet know the fingerprint
	// at dial time; PeerName carries the alias for those rows and
	// Peer stays empty until fingerprint discovery lands in 3B.2.
	Peer      PeerID    `json:"peer,omitempty"`
	PeerName  string    `json:"peer_name,omitempty"`
	Endpoint  Endpoint  `json:"endpoint"`
	Transport string    `json:"transport"` // "ssh", "http-probe", "cloudbox-ssh", "lan-direct-ssh"
	LatencyMs int64     `json:"latency_ms"`
	At        time.Time `json:"at,omitzero"`
	Source    Source    `json:"source,omitempty"` // "" when locally observed; SourceGossip when received from a peer
}
