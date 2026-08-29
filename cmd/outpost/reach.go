// `outpost reach [user@]host` — one-shot reachability probe with a
// machine-readable verdict ("lan" | "cloudbox" | "offline") and a
// stable exit code (0 | 10 | 20). Scripts call it before deciding
// whether to dial LAN-direct or fall through cloudbox.
//
// The probe deliberately stops before the SSH handshake so it's both
// fast (<2 s) and side-effect-free (no elevation cookie required, no
// /auth password challenge). LAN classification proves the peer's
// announced LAN endpoint is currently accepting connections; cloudbox
// classification proves the matrix portal is reachable from this
// machine; offline classification means neither path is currently
// usable.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/discovery"
	"github.com/qiangli/outpost/internal/agent/peerstatus"
)

// ReachResult is the JSON shape emitted on stdout. Stable for
// scripting; any field addition must be additive.
type ReachResult struct {
	Host     string     `json:"host"`
	Route    string     `json:"route"`              // "lan" | "mesh" | "cloudbox" | "offline"
	RTTMs    int64      `json:"rtt_ms"`             // for lan + cloudbox; 0 on offline
	Endpoint string     `json:"endpoint,omitempty"` // host:port for lan, scheme://host[:port] for cloudbox
	LastSeen *time.Time `json:"last_seen,omitempty"`
	Detail   string     `json:"detail,omitempty"` // why we landed on cloudbox/offline
}

const (
	reachExitLAN      = 0
	reachExitCloudbox = 10
	reachExitOffline  = 20
)

// reachExitMesh deliberately EQUALS reachExitLAN. Both mean "a direct path to
// this host exists", which is the question preflights actually ask — they are
// written as `if ! outpost reach h; then skip; fi`. Giving mesh its own
// non-zero code would make such a preflight skip a host it can reach directly.
// Callers that need to tell the two apart read the `route` field, which is
// where the distinction belongs. Nothing regresses: every host that now
// reports mesh previously reported cloudbox (exit 10), so this can only turn a
// wrong "skip" into a correct "proceed".
const reachExitMesh = reachExitLAN

func reachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reach [user@]host",
		Short: "Classify reachability to a paired host as lan|mesh|cloudbox|offline",
		Long: `outpost reach [user@]host

One-shot probe; emits a single JSON line on stdout and exits with a
stable code:

  exit 0   lan        — peer's announced LAN endpoint accepted TCP
  exit 0   mesh       — a live DIRECT peer-to-peer link to the host is up
  exit 10  cloudbox   — no direct path; cloudbox matrix portal is up
  exit 20  offline    — neither a direct path nor cloudbox is reachable

lan and mesh share exit 0 because both mean "reachable directly"; the
JSON route field distinguishes them.

Designed for shell preflights:

  if ! outpost reach host-b >/dev/null; then
    echo "host-b unreachable — skipping deploy"; exit 1
  fi

Bails before the SSH handshake — no password prompt, no elevation
cookie required, no side effects on the reachability ledger.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, host := parseUserAtHost(args[0])
			if host == "" {
				return errors.New("reach: empty host")
			}
			res := ProbeReachability(cmd.Context(), host, 2*time.Second)
			// Always emit one JSON line on stdout, even on
			// offline — scripts parse the same field regardless.
			b, _ := json.Marshal(res)
			fmt.Println(string(b))
			switch res.Route {
			case "lan":
				os.Exit(reachExitLAN)
			case "mesh":
				os.Exit(reachExitMesh)
			case "cloudbox":
				os.Exit(reachExitCloudbox)
			default:
				os.Exit(reachExitOffline)
			}
			return nil
		},
	}
}

// ProbeReachability returns the LAN/cloudbox/offline verdict for
// `host`. Bounded by `timeout` overall; each individual dial uses a
// short sub-timeout so a slow LAN miss doesn't starve the cloudbox
// probe. Pure read: no elevation, no SSH handshake, no ledger writes.
func ProbeReachability(ctx context.Context, host string, timeout time.Duration) ReachResult {
	res := ReachResult{Host: host}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// LAN ledger lookup is best-effort. A missing ledger or read
	// error just means we don't surface last_seen — the classification
	// itself doesn't depend on it.
	if t := lastSeenFromLedger(host); t != nil {
		res.LastSeen = t
	}

	// Step 1: try LAN. mDNS browse + TCP-handshake to the announced
	// EndpointLANSSHWS. Capped at ~1.2s so we leave budget for the
	// cloudbox probe.
	if rtt, ep, ok := probeLAN(ctx, host); ok {
		res.Route = "lan"
		res.RTTMs = rtt
		res.Endpoint = ep
		return res
	}

	// Step 1b: try the mesh. The LAN rung above is an mDNS browse followed by
	// a dial of a self-advertised endpoint, and it goes dark in three ordinary
	// situations: the peer has discovery disabled, a router sits between the
	// peers (mDNS does not cross subnets), or multicast simply is not
	// delivered on the network. In every one of those the libp2p mesh may
	// ALREADY hold a direct, hole-punched connection to the very host being
	// probed — measured: a whole site where all three pairs held direct links
	// and all three classified `cloudbox`, including two hosts sharing a /24.
	//
	// So ask the transport instead of re-deriving locality. An existing direct
	// connection is proof of reachability that needs no discovery. Only a
	// DIRECT link counts: a relayed mesh circuit is not a local route and must
	// not outrank cloudbox.
	if rtt, ep, ok := probeMesh(ctx, host); ok {
		res.Route = "mesh"
		res.RTTMs = rtt
		res.Endpoint = ep
		return res
	}

	// Step 2: try cloudbox. TLS handshake only — no HTTP body, no
	// `/healthz` fetch. Confirms TCP+TLS reachability, which is
	// what every cloudbox-tunneled outpost path requires before
	// the first byte of the wss upgrade.
	if rtt, ep, err := probeCloudbox(ctx, host); err == nil {
		res.Route = "cloudbox"
		res.RTTMs = rtt
		res.Endpoint = ep
		return res
	} else {
		res.Detail = err.Error()
	}

	res.Route = "offline"
	return res
}

func probeLAN(ctx context.Context, host string) (rttMs int64, endpoint string, ok bool) {
	browseCtx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	peers, _ := discovery.Browse(browseCtx, discovery.BrowseOptions{Timeout: 1200 * time.Millisecond})
	peer := findMatchingPeer(peers, host)
	if peer == nil {
		return 0, "", false
	}
	ep := peer.FirstEndpoint(discovery.EndpointLANSSHWS)
	if ep.Host == "" || ep.Port == 0 {
		return 0, "", false
	}

	hostPort := ep.HostPort()
	start := time.Now()
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		return 0, "", false
	}
	_ = conn.Close()
	return time.Since(start).Milliseconds(), hostPort, true
}

// probeMesh reports a live DIRECT mesh link to host, asking the local daemon
// for state it already has. It performs no dial and no discovery, so it costs
// one loopback MCP call and is immune to the multicast/flag/subnet conditions
// that silence probeLAN.
//
// Any error — mesh off, daemon not running, tool absent on an older daemon —
// is a negative answer, never a hard failure: reach must still classify.
func probeMesh(ctx context.Context, host string) (rttMs int64, endpoint string, ok bool) {
	mctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()

	start := time.Now()
	out, ok := meshLinkFor(mctx, host)
	// Retry on "the mesh does not know this NAME", which is a successful call
	// reporting Found=false — not on !ok, which means the call itself failed.
	if !ok || !out.Link.Found {
		// The daemon's rendezvous now canonicalizes alias → registered name
		// itself (mesh.Rendezvous.CanonicalHost, learned from the same peers
		// listing it already fetches), so on a current daemon a single lookup
		// answers for either spelling. This retry is the fallback for an OLDER
		// daemon that still keys only on the registered name: there
		// `reach <alias>` silently fell through to the relay while
		// `reach <agent-name>` took the direct link — same host, opposite
		// verdict, precisely the downgrade this rung exists to remove.
		// Cloudbox knows both spellings; ask it once.
		if alt := meshAliasFor(ctx, host); alt != "" {
			// Fresh deadline: mctx has already spent its budget on the first
			// lookup and the alias fetch, so reusing it would cancel the retry
			// before it left the door.
			actx, acancel := context.WithTimeout(ctx, 800*time.Millisecond)
			out, ok = meshLinkFor(actx, alt)
			acancel()
		}
	}
	if !ok || !out.Link.Found || !out.Link.Direct {
		return 0, "", false
	}
	// Report the peer id rather than a raw remote address: it is the stable
	// identifier of the path, and it is what `outpost mesh` commands take.
	ep := "mesh:" + out.Link.PeerID
	if out.Link.LinkClass != "" {
		ep += " (link=" + out.Link.LinkClass + ")"
	}
	return time.Since(start).Milliseconds(), ep, true
}

// meshLinkFor asks the local daemon for a live link to one spelling of a host.
func meshLinkFor(ctx context.Context, host string) (struct {
	Link admincore.MeshHostLinkView `json:"link"`
}, bool) {
	var out struct {
		Link admincore.MeshHostLinkView `json:"link"`
	}
	if host == "" {
		return out, false
	}
	if err := runMeshTool(ctx, "outpost_mesh_link", struct {
		Host string `json:"host"`
	}{Host: host}, &out); err != nil {
		return out, false
	}
	return out, true
}

// meshAliasFor returns the OTHER spelling cloudbox knows for host — its alias
// when given the host name, or its host name when given the alias. Empty when
// the question cannot be answered; the caller then simply keeps its verdict.
func meshAliasFor(ctx context.Context, host string) string {
	cfgPath, err := conf.DefaultConfigPath()
	if err != nil {
		return ""
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil || fc == nil || fc.AccessToken == "" {
		return ""
	}
	base := cloudboxHTTPBase(fc)
	if base == "" {
		return ""
	}
	pctx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	peers, err := peerstatus.Fetch(pctx, base, fc.AccessToken, &http.Client{Timeout: 700 * time.Millisecond})
	if err != nil {
		return ""
	}
	for _, p := range peers {
		if strings.EqualFold(p.Host, host) && p.Alias != "" {
			return p.Alias
		}
		if strings.EqualFold(p.Alias, host) && p.Host != "" {
			return p.Host
		}
	}
	return ""
}

// probeCloudbox confirms the relay is up AND that cloudbox believes `host` is
// online. Confirming only the relay was a real defect: a host whose tunnel is
// down still classified `cloudbox`, so a preflight saw a usable route and the
// very next `outpost ssh` failed with HTTP 504. The rung reported on the wrong
// thing — the same error the LAN rung makes with mDNS.
//
// The peer check DOWNGRADES only on an affirmative negative. If the peers API
// cannot be reached or does not know the host, the verdict stays `cloudbox`:
// this probe must not invent an outage from its own inability to ask.
func probeCloudbox(ctx context.Context, host string) (rttMs int64, endpoint string, err error) {
	cfgPath, perr := conf.DefaultConfigPath()
	if perr != nil {
		return 0, "", fmt.Errorf("locate config: %w", perr)
	}
	fc, lerr := conf.LoadFile(cfgPath)
	if lerr != nil {
		return 0, "", fmt.Errorf("load config: %w", lerr)
	}
	if fc == nil || fc.ServerAddr == "" {
		return 0, "", errors.New("local outpost is not paired with cloudbox")
	}
	base := cloudboxHTTPBase(fc)
	if base == "" {
		return 0, "", errors.New("cloudbox base URL not derivable from config")
	}

	u, perr := url.Parse(base)
	if perr != nil {
		return 0, "", fmt.Errorf("parse cloudbox URL: %w", perr)
	}
	hostPort := u.Host
	if !strings.Contains(hostPort, ":") {
		if u.Scheme == "https" {
			hostPort += ":443"
		} else {
			hostPort += ":80"
		}
	}

	start := time.Now()
	d := net.Dialer{Timeout: 1000 * time.Millisecond}
	conn, derr := d.DialContext(ctx, "tcp", hostPort)
	if derr != nil {
		return 0, base, fmt.Errorf("dial cloudbox %s: %w", hostPort, derr)
	}
	defer conn.Close()

	if u.Scheme == "https" {
		// TLS handshake confirms cloudbox is actually serving on this
		// port (a stray TCP listener wouldn't complete the handshake).
		tlsConn := tls.Client(conn, &tls.Config{ServerName: u.Hostname()})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return 0, base, fmt.Errorf("tls handshake to cloudbox: %w", err)
		}
		_ = tlsConn.Close()
	}
	rtt := time.Since(start).Milliseconds()

	if online, known := cloudboxSaysOnline(ctx, fc, base, host); known && !online {
		return 0, base, fmt.Errorf("cloudbox is up but reports %q offline", host)
	}
	return rtt, base, nil
}

// cloudboxSaysOnline asks cloudbox whether host is currently connected.
// Returns known=false whenever the question could not be answered — no token,
// transport error, host absent from the listing — so the caller leaves its
// verdict unchanged.
func cloudboxSaysOnline(ctx context.Context, fc *conf.FileConfig, base, host string) (online, known bool) {
	if fc == nil || fc.AccessToken == "" {
		return false, false
	}
	pctx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	peers, perr := peerstatus.Fetch(pctx, base, fc.AccessToken, &http.Client{Timeout: 700 * time.Millisecond})
	if perr != nil {
		return false, false
	}
	for _, p := range peers {
		if strings.EqualFold(p.Host, host) {
			return p.Online, true
		}
	}
	return false, false
}

// lastSeenFromLedger scans the reachability ledger for the most
// recent successful edge to `host` (matched by PeerName). Returns nil
// if the ledger doesn't exist or has no matching entries.
func lastSeenFromLedger(host string) *time.Time {
	path, err := discovery.DefaultLedgerPath()
	if err != nil {
		return nil
	}
	l, err := discovery.OpenLedger(path)
	if err != nil {
		return nil
	}
	edges, err := l.Tail(0)
	if err != nil || len(edges) == 0 {
		return nil
	}
	var newest *time.Time
	lower := strings.ToLower(host)
	for i := range edges {
		e := &edges[i]
		if strings.ToLower(e.PeerName) != lower {
			continue
		}
		if newest == nil || e.At.After(*newest) {
			t := e.At
			newest = &t
		}
	}
	return newest
}
