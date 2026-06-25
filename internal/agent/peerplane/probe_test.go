package peerplane

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		rtt  float64
		want Tier
	}{
		{0.5, TierTP}, {2.0, TierTP}, {2.1, TierLAN},
		{15, TierLAN}, {20, TierLAN}, {20.1, TierWAN}, {120, TierWAN},
	}
	for _, c := range cases {
		if got := Classify(c.rtt); got != c.want {
			t.Errorf("Classify(%v)=%v, want %v", c.rtt, got, c.want)
		}
	}
}

func TestLocalCandidates(t *testing.T) {
	for _, c := range LocalCandidates(9999) {
		if strings.HasPrefix(c, "127.") {
			t.Errorf("loopback leaked into candidates: %s", c)
		}
		if !strings.HasSuffix(c, ":9999") {
			t.Errorf("candidate missing announce port: %s", c)
		}
	}
}

func TestProbe_AgainstEchoResponder(t *testing.T) {
	r, err := NewEchoResponder(0)
	if err != nil {
		t.Fatalf("responder: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(r.Port()))
	res := ProbeCandidate(addr, 3)
	if !res.Reachable || res.Tier == "" {
		t.Fatalf("expected reachable+tiered, got %+v", res)
	}
	if res.Tier != TierTP && res.Tier != TierLAN {
		t.Errorf("loopback probe tier=%v, expected TP/LAN", res.Tier)
	}
	if dead := ProbeCandidate("127.0.0.1:2", 1); dead.Reachable {
		t.Errorf("expected unreachable for dead port, got %+v", dead)
	}
}

func TestProbeTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if r := ProbeTCP(ln.Addr().String(), 3); !r.Reachable {
		t.Fatalf("expected reachable, got %+v", r)
	}
	if r := ProbeTCP("127.0.0.1:2", 1); r.Reachable {
		t.Errorf("expected unreachable, got %+v", r)
	}
}

// ProbeAll picks the lowest-RTT reachable candidate and leaves unreachable ones
// marked.
func TestProbeAll(t *testing.T) {
	r, _ := NewEchoResponder(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	live := net.JoinHostPort("127.0.0.1", strconv.Itoa(r.Port()))

	results, best := ProbeAll([]string{"127.0.0.1:2", live}, 2, ProbeCandidate)
	if len(results) != 2 {
		t.Fatalf("results=%d", len(results))
	}
	if best == nil || best.Addr != live {
		t.Fatalf("best=%+v, want %s", best, live)
	}
}
