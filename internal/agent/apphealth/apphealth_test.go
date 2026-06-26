package apphealth

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
)

func TestClassify(t *testing.T) {
	tcs := []struct {
		rtt  float64
		want string
	}{
		{0.5, TierTP},
		{2.0, TierTP},
		{2.1, TierLAN},
		{15.0, TierLAN},
		{20.0, TierLAN},
		{20.1, TierWAN},
		{120.0, TierWAN},
	}
	for _, tc := range tcs {
		if got := Classify(tc.rtt); got != tc.want {
			t.Errorf("Classify(%.1f)=%q, want %q", tc.rtt, got, tc.want)
		}
	}
}

func TestProbeHTTP_Reachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := probeHTTP(srv.Client(), srv.URL)
	if !r.Reachable {
		t.Fatalf("expected reachable, got %+v", r)
	}
	if r.RTTms <= 0 {
		t.Errorf("expected positive RTT, got %.3f", r.RTTms)
	}
	if r.Tier == "" || r.Tier == TierUnreached {
		t.Errorf("expected tier, got %q", r.Tier)
	}
	if r.StatusCode != 200 {
		t.Errorf("status=%d, want 200", r.StatusCode)
	}
}

func TestProbeHTTP_Unreachable(t *testing.T) {
	r := probeHTTP(http.DefaultClient, "http://127.0.0.1:1")
	if r.Reachable {
		t.Fatalf("expected unreachable, got %+v", r)
	}
	if r.Tier != TierUnreached {
		t.Errorf("tier=%q, want %q", r.Tier, TierUnreached)
	}
	if r.Error == "" {
		t.Error("expected non-empty error")
	}
}

func TestProbeHTTP_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := probeHTTP(srv.Client(), srv.URL)
	if r.Reachable {
		t.Fatalf("expected unreachable for 5xx, got %+v", r)
	}
	if r.Tier != TierUnreached {
		t.Errorf("tier=%q, want %q", r.Tier, TierUnreached)
	}
	if r.StatusCode != 500 {
		t.Errorf("status=%d, want 500", r.StatusCode)
	}
}

func TestProbeHTTP_404_IsReachable(t *testing.T) {
	// 4xx responses mean the server is reachable — the app is running
	// even if the specific endpoint doesn't exist.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	r := probeHTTP(srv.Client(), srv.URL)
	if !r.Reachable {
		t.Fatalf("expected reachable for 404, got %+v", r)
	}
	if r.Tier == TierUnreached {
		t.Errorf("expected tier, got %q", r.Tier)
	}
}

func TestProbeTCP_Reachable(t *testing.T) {
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
			c.Close()
		}
	}()

	r := probeTCP(ln.Addr().String())
	if !r.Reachable {
		t.Fatalf("expected reachable, got %+v", r)
	}
	if r.RTTms <= 0 {
		t.Errorf("expected positive RTT, got %.3f", r.RTTms)
	}
	if r.Tier == "" || r.Tier == TierUnreached {
		t.Errorf("expected tier, got %q", r.Tier)
	}
}

func TestProbeTCP_Unreachable(t *testing.T) {
	r := probeTCP("127.0.0.1:1")
	if r.Reachable {
		t.Fatalf("expected unreachable, got %+v", r)
	}
	if r.Tier != TierUnreached {
		t.Errorf("tier=%q, want %q", r.Tier, TierUnreached)
	}
	if r.Error == "" {
		t.Error("expected non-empty error")
	}
}

func TestService_Snapshot_Sorted(t *testing.T) {
	cfg := Config{
		Apps:       agent.NewAppRegistry(),
		Interval:   time.Hour,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
	}
	svc := New(cfg)

	svc.mu.Lock()
	svc.state["zulu"] = ProbeResult{Name: "zulu", Tier: TierWAN, Reachable: true}
	svc.state["alpha"] = ProbeResult{Name: "alpha", Tier: TierTP, Reachable: true}
	svc.state["charlie"] = ProbeResult{Name: "charlie", Tier: TierLAN, Reachable: true}
	svc.mu.Unlock()

	snap := svc.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot len=%d, want 3", len(snap))
	}
	if !sort.SliceIsSorted(snap, func(i, j int) bool { return snap[i].Name < snap[j].Name }) {
		t.Errorf("snapshot not sorted by name: %v", snap)
	}
}

func TestService_Cycle_NoApps(t *testing.T) {
	cfg := Config{
		Apps:       agent.NewAppRegistry(),
		Interval:   time.Hour,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
	}
	svc := New(cfg)
	svc.cycle()
	snap := svc.Snapshot()
	if len(snap) != 0 {
		t.Errorf("expected empty snapshot, got %d entries", len(snap))
	}
}

func TestService_Run_Cancellation(t *testing.T) {
	cfg := Config{
		Apps:       agent.NewAppRegistry(),
		Interval:   10 * time.Millisecond,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
	}
	svc := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned error: %v", err)
	}
}

func TestProbeTCP_RTT_Positive(t *testing.T) {
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
			c.Close()
		}
	}()

	r := probeTCP(ln.Addr().String())
	if !r.Reachable {
		t.Fatalf("expected reachable, got %+v", r)
	}
	// Loopback RTT should be well under 100ms.
	if r.RTTms > 100 {
		t.Errorf("loopback RTT too high: %.3f ms", r.RTTms)
	}
}

func TestService_Cycle_WithHTTPApp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	reg := agent.NewAppRegistry()
	if err := reg.Register("test-app", srv.URL); err != nil {
		t.Fatalf("register: %v", err)
	}

	cfg := Config{
		Apps:       reg,
		Interval:   time.Hour,
		HTTPClient: srv.Client(),
	}
	svc := New(cfg)
	svc.cycle()

	snap := svc.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len=%d, want 1", len(snap))
	}
	r := snap[0]
	if r.Name != "test-app" {
		t.Errorf("name=%q, want %q", r.Name, "test-app")
	}
	if !r.Reachable {
		t.Fatalf("expected reachable, got %+v", r)
	}
	if r.Tier == TierUnreached {
		t.Errorf("unexpected tier %q", r.Tier)
	}
	if r.RTTms <= 0 {
		t.Errorf("expected positive RTT, got %.3f", r.RTTms)
	}
}

func TestService_Cycle_WithTCPApp(t *testing.T) {
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
			c.Close()
		}
	}()

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	reg := agent.NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name:    "tcp-app",
		Scheme:  "tcp",
		Host:    "127.0.0.1",
		Port:    port,
		Enabled: true,
	}); err != nil {
		t.Fatalf("registerFromConfig: %v", err)
	}

	cfg := Config{
		Apps:       reg,
		Interval:   time.Hour,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
	}
	svc := New(cfg)
	svc.cycle()

	snap := svc.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len=%d, want 1", len(snap))
	}
	r := snap[0]
	if r.Name != "tcp-app" {
		t.Errorf("name=%q, want %q", r.Name, "tcp-app")
	}
	if !r.Reachable {
		t.Fatalf("expected reachable, got %+v", r)
	}
	if r.Tier == TierUnreached {
		t.Errorf("unexpected tier %q", r.Tier)
	}
}

func TestService_Cycle_WithDeadApp(t *testing.T) {
	reg := agent.NewAppRegistry()
	if err := reg.Register("dead-app", "http://127.0.0.1:1"); err != nil {
		t.Fatalf("register: %v", err)
	}

	cfg := Config{
		Apps:       reg,
		Interval:   time.Hour,
		HTTPClient: &http.Client{Timeout: 500 * time.Millisecond},
	}
	svc := New(cfg)
	svc.cycle()

	snap := svc.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len=%d, want 1", len(snap))
	}
	r := snap[0]
	if r.Reachable {
		t.Fatalf("expected unreachable, got %+v", r)
	}
	if r.Tier != TierUnreached {
		t.Errorf("tier=%q, want %q", r.Tier, TierUnreached)
	}
}
