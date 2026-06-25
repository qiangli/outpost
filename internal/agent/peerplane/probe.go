// Package peerplane is the outpost side of the dhnt p2p resource fabric's peer
// data plane. cloudbox is the sole rendezvous (no third-party discovery); this
// package announces the host's interface candidates to cloudbox, runs a probe
// responder, and measures RTT to peers to classify each link into a sharding
// tier — the "measure, don't guess" locality signal that finds the dedicated-
// LAN/hub path the egress-IP heuristic and single-interface mDNS miss.
//
// See docs/p2p-resource-fabric-design.md (umbrella).
package peerplane

import (
	"fmt"
	"net"
	"time"
)

// Tier thresholds (round-trip ms). Heuristic, tunable.
const (
	tpMaxRTT  = 2.0  // <=2ms: dedicated/wired LAN — tensor-parallel eligible
	lanMaxRTT = 20.0 // <=20ms: general LAN (incl. wifi) — pipeline-parallel
)

// Tier is the measured locality class of a peer link.
type Tier string

const (
	TierTP        Tier = "tp"        // tensor-parallel eligible (sub-2ms, dedicated/wired)
	TierLAN       Tier = "lan"       // pipeline-parallel (LAN/wifi)
	TierWAN       Tier = "wan"       // pipeline / relay only
	TierUnreached Tier = "unreached" // no direct path — relay required
)

// Classify maps a measured RTT (ms) to a sharding tier.
func Classify(rttMS float64) Tier {
	switch {
	case rttMS <= tpMaxRTT:
		return TierTP
	case rttMS <= lanMaxRTT:
		return TierLAN
	default:
		return TierWAN
	}
}

// LocalCandidates returns this host's non-loopback IPv4 addresses as "ip:port"
// — EVERY interface (wifi, ethernet hub, link-local). Announcing all of them is
// the point: a peer can then find the best path (e.g. a direct hub link-local)
// that no single-interface view would surface.
func LocalCandidates(port int) []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP.To4()
		if ip == nil || ip.IsLoopback() {
			continue
		}
		out = append(out, fmt.Sprintf("%s:%d", ip.String(), port))
	}
	return out
}

// Result is one candidate's measurement.
type Result struct {
	Addr      string  `json:"addr"`
	RTT       float64 `json:"rtt_ms"` // valid only when Reachable
	Reachable bool    `json:"reachable"`
	Tier      Tier    `json:"tier"`
}

// ProbeCandidate measures the min RTT (ms) to a candidate by k UDP ping/pong
// round-trips against a peer's EchoResponder. Direct-dials its own socket, so
// it's independent of any receive loop. Unreachable (no reply within the
// per-ping deadline) ⇒ Reachable=false — the relay/WAN case for a NAT'd peer.
func ProbeCandidate(addr string, k int) Result {
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return Result{Addr: addr, Tier: TierUnreached}
	}
	c, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return Result{Addr: addr, Tier: TierUnreached}
	}
	defer c.Close()
	best := -1.0
	buf := make([]byte, 16)
	for i := 0; i < k; i++ {
		t0 := time.Now()
		if _, err := c.Write([]byte("ping")); err != nil {
			continue
		}
		_ = c.SetReadDeadline(time.Now().Add(800 * time.Millisecond))
		if _, err := c.Read(buf); err != nil {
			continue
		}
		rtt := float64(time.Since(t0).Microseconds()) / 1000.0
		if best < 0 || rtt < best {
			best = rtt
		}
	}
	if best < 0 {
		return Result{Addr: addr, Tier: TierUnreached}
	}
	return Result{Addr: addr, RTT: best, Reachable: true, Tier: Classify(best)}
}

// ProbeTCP measures RTT to addr by timing a TCP connect (k attempts, min RTT).
// Needs NO peer cooperation — it works against any host with an open port (e.g.
// ssh :22), so it can tier the fleet against existing services.
func ProbeTCP(addr string, k int) Result {
	best := -1.0
	for i := 0; i < k; i++ {
		t0 := time.Now()
		c, err := net.DialTimeout("tcp", addr, 800*time.Millisecond)
		if err != nil {
			continue
		}
		rtt := float64(time.Since(t0).Microseconds()) / 1000.0
		_ = c.Close()
		if best < 0 || rtt < best {
			best = rtt
		}
	}
	if best < 0 {
		return Result{Addr: addr, Tier: TierUnreached}
	}
	return Result{Addr: addr, RTT: best, Reachable: true, Tier: Classify(best)}
}

// ProbeAll measures every candidate concurrently with fn and returns the
// results plus the best (lowest-RTT reachable) one (nil if none reachable).
func ProbeAll(cands []string, k int, fn func(string, int) Result) (results []Result, best *Result) {
	type rc struct {
		i int
		r Result
	}
	ch := make(chan rc, len(cands))
	for i, a := range cands {
		go func(i int, a string) { ch <- rc{i, fn(a, k)} }(i, a)
	}
	results = make([]Result, len(cands))
	for range cands {
		x := <-ch
		results[x.i] = x.r
	}
	for i := range results {
		r := &results[i]
		if r.Reachable && (best == nil || r.RTT < best.RTT) {
			best = r
		}
	}
	return results, best
}
