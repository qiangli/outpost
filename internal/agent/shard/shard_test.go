package shard

import (
	"fmt"
	"net"
	"reflect"
	"testing"
)

// ring2 / ring3 build canonical placements; the local member's peer id is
// irrelevant (PlanFor uses myRank), so every member carries a realistic id.
func ring2() Ring {
	return Ring{Members: []Member{
		{Rank: 0, Host: "leader", PeerID: "peer-0"},
		{Rank: 1, Host: "worker", PeerID: "peer-1"},
	}}
}

func ring3() Ring {
	return Ring{Members: []Member{
		{Rank: 2, Host: "c", PeerID: "peer-2"},
		{Rank: 0, Host: "a", PeerID: "peer-0"},
		{Rank: 1, Host: "b", PeerID: "peer-1"},
	}}
}

func fwdSet(p *HostPlan) map[string][2]string {
	m := map[string][2]string{}
	for _, f := range p.Forwards {
		m[f.LocalAddr] = [2]string{f.PeerID, f.Service}
	}
	return m
}

func exposeSet(p *HostPlan) map[string]string {
	m := map[string]string{}
	for _, e := range p.Exposes {
		m[e.Service] = e.Addr
	}
	return m
}

func TestPlanFor2Rank_Leader(t *testing.T) {
	p, err := ring2().PlanFor(0)
	if err != nil {
		t.Fatal(err)
	}
	if p.World != 2 || p.MyRank != 0 || p.DataPort != DefaultDataPort {
		t.Fatalf("bad header: %+v", p)
	}
	// Leader binds its own data/signal.
	wantExp := map[string]string{
		DataService:   "127.0.0.1:9000",
		SignalService: "127.0.0.1:10000",
	}
	if got := exposeSet(p); !reflect.DeepEqual(got, wantExp) {
		t.Errorf("exposes = %v, want %v", got, wantExp)
	}
	// Forwards to the next rank only (master-spoke skipped for rank 0).
	wantFwd := map[string][2]string{
		"127.0.0.1:9001":  {"peer-1", DataService},
		"127.0.0.1:10001": {"peer-1", SignalService},
	}
	if got := fwdSet(p); !reflect.DeepEqual(got, wantFwd) {
		t.Errorf("forwards = %v, want %v", got, wantFwd)
	}
}

func TestPlanFor2Rank_Worker_DedupesNextEqualsMaster(t *testing.T) {
	p, err := ring2().PlanFor(1)
	if err != nil {
		t.Fatal(err)
	}
	// Worker binds its own data/signal.
	wantExp := map[string]string{
		DataService:   "127.0.0.1:9001",
		SignalService: "127.0.0.1:10001",
	}
	if got := exposeSet(p); !reflect.DeepEqual(got, wantExp) {
		t.Errorf("exposes = %v, want %v", got, wantExp)
	}
	// next==0==master, so ring-data and master-spoke collapse to ONE forward at
	// 127.0.0.1:9000; plus the ring-signal. Exactly two forwards (no duplicate).
	wantFwd := map[string][2]string{
		"127.0.0.1:9000":  {"peer-0", DataService},
		"127.0.0.1:10000": {"peer-0", SignalService},
	}
	if got := fwdSet(p); !reflect.DeepEqual(got, wantFwd) {
		t.Errorf("forwards = %v, want %v", got, wantFwd)
	}
	if len(p.Forwards) != 2 {
		t.Errorf("dedupe failed: got %d forwards, want 2", len(p.Forwards))
	}
}

func TestPlanFor3Rank_MiddleRank_HasRingAndMasterSpoke(t *testing.T) {
	p, err := ring3().PlanFor(1) // middle rank: next=2, master=0 (distinct)
	if err != nil {
		t.Fatal(err)
	}
	wantFwd := map[string][2]string{
		"127.0.0.1:9002":  {"peer-2", DataService},   // ring data → next(2)
		"127.0.0.1:10002": {"peer-2", SignalService}, // ring signal → next(2)
		"127.0.0.1:9000":  {"peer-0", DataService},   // master-spoke → rank0
	}
	if got := fwdSet(p); !reflect.DeepEqual(got, wantFwd) {
		t.Errorf("forwards = %v, want %v", got, wantFwd)
	}
}

func TestPlanFor_CustomPorts(t *testing.T) {
	r := ring2()
	r.DataPort = 7000
	r.SignalPort = 8000
	p, err := r.PlanFor(0)
	if err != nil {
		t.Fatal(err)
	}
	if got := exposeSet(p)[DataService]; got != "127.0.0.1:7000" {
		t.Errorf("data expose = %s, want 127.0.0.1:7000", got)
	}
	if got := fwdSet(p)["127.0.0.1:8001"]; got != ([2]string{"peer-1", SignalService}) {
		t.Errorf("signal forward not at custom base: %v", fwdSet(p))
	}
}

func TestPlanFor_Validation(t *testing.T) {
	cases := []struct {
		name string
		ring Ring
		rank int
	}{
		{"too few members", Ring{Members: []Member{{Rank: 0, PeerID: "p"}}}, 0},
		{"non-contiguous ranks", Ring{Members: []Member{{Rank: 0, PeerID: "a"}, {Rank: 2, PeerID: "b"}}}, 0},
		{"rank out of range", ring2(), 5},
		{"next missing peer id", Ring{Members: []Member{{Rank: 0, PeerID: "a"}, {Rank: 1, PeerID: ""}}}, 0},
		{"master missing peer id", Ring{Members: []Member{{Rank: 0, PeerID: ""}, {Rank: 1, PeerID: "b"}}}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.ring.PlanFor(c.rank); err == nil {
				t.Errorf("expected error for %s, got nil", c.name)
			}
		})
	}
}

func TestFullArgs(t *testing.T) {
	p, _ := ring2().PlanFor(0)
	got := p.FullArgs("/models/qwen.gguf", "--prefetch", "-p", "hi", "-n", "16")
	want := []string{
		"--world", "2", "--rank", "0", "--master", "127.0.0.1", "--next", "127.0.0.1",
		"--data-port", "9000", "-m", "/models/qwen.gguf", "--prefetch", "-p", "hi", "-n", "16",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FullArgs = %v\nwant %v", got, want)
	}
}

// fakeForwarder records Expose/Unexpose/Listen and can fail the Nth Listen.
type fakeForwarder struct {
	exposed map[string]string
	opened  []*fakeListener
	failAt  int // 1-based; 0 = never fail
	n       int
}

func newFake() *fakeForwarder { return &fakeForwarder{exposed: map[string]string{}} }

func (f *fakeForwarder) Expose(s, a string) { f.exposed[s] = a }
func (f *fakeForwarder) Unexpose(s string)  { delete(f.exposed, s) }
func (f *fakeForwarder) Listen(local, peer, svc string) (net.Listener, error) {
	f.n++
	if f.failAt != 0 && f.n == f.failAt {
		return nil, fmt.Errorf("listen %s: boom", local)
	}
	ln := &fakeListener{addr: local}
	f.opened = append(f.opened, ln)
	return ln, nil
}

type fakeListener struct {
	addr   string
	closed bool
}

func (l *fakeListener) Accept() (net.Conn, error) { return nil, fmt.Errorf("closed") }
func (l *fakeListener) Close() error              { l.closed = true; return nil }
func (l *fakeListener) Addr() net.Addr            { return fakeAddr(l.addr) }

type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

func TestApply_WiresAndCleansUp(t *testing.T) {
	f := newFake()
	p, _ := ring2().PlanFor(1) // worker: 2 exposes, 2 forwards
	cleanup, err := Apply(f, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.exposed) != 2 {
		t.Errorf("expected 2 exposed services, got %d (%v)", len(f.exposed), f.exposed)
	}
	if len(f.opened) != 2 {
		t.Errorf("expected 2 listeners, got %d", len(f.opened))
	}
	cleanup()
	if len(f.exposed) != 0 {
		t.Errorf("cleanup did not unexpose: %v", f.exposed)
	}
	for i, ln := range f.opened {
		if !ln.closed {
			t.Errorf("listener %d (%s) not closed by cleanup", i, ln.addr)
		}
	}
}

func TestApply_FailClosed(t *testing.T) {
	f := newFake()
	f.failAt = 2               // fail the second Listen
	p, _ := ring3().PlanFor(1) // 3 forwards → the failure is mid-way
	cleanup, err := Apply(f, p)
	if err == nil {
		t.Fatal("expected Apply to fail")
	}
	if cleanup != nil {
		t.Error("cleanup should be nil on failure")
	}
	// Everything opened before the failure must be torn down, services unexposed.
	if len(f.exposed) != 0 {
		t.Errorf("fail-closed left services exposed: %v", f.exposed)
	}
	for i, ln := range f.opened {
		if !ln.closed {
			t.Errorf("fail-closed left listener %d (%s) open", i, ln.addr)
		}
	}
}
