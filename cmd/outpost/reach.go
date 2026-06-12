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
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/discovery"
)

// ReachResult is the JSON shape emitted on stdout. Stable for
// scripting; any field addition must be additive.
type ReachResult struct {
	Host     string     `json:"host"`
	Route    string     `json:"route"`              // "lan" | "cloudbox" | "offline"
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

func reachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reach [user@]host",
		Short: "Classify reachability to a paired host as lan|cloudbox|offline",
		Long: `outpost reach [user@]host

One-shot probe; emits a single JSON line on stdout and exits with a
stable code:

  exit 0   lan        — peer's announced LAN endpoint accepted TCP
  exit 10  cloudbox   — LAN unreachable; cloudbox matrix portal is up
  exit 20  offline    — neither LAN nor cloudbox is currently reachable

Designed for shell preflights:

  if ! outpost reach novicortex >/dev/null; then
    echo "novicortex unreachable — skipping deploy"; exit 1
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

	// Step 2: try cloudbox. TLS handshake only — no HTTP body, no
	// `/healthz` fetch. Confirms TCP+TLS reachability, which is
	// what every cloudbox-tunneled outpost path requires before
	// the first byte of the wss upgrade.
	if rtt, ep, err := probeCloudbox(ctx); err == nil {
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

func probeCloudbox(ctx context.Context) (rttMs int64, endpoint string, err error) {
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
	return time.Since(start).Milliseconds(), base, nil
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
