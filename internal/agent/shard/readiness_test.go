package shard

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// capLog is a slog.Handler that records messages + attrs for assertions.
type capLog struct {
	mu   sync.Mutex
	recs []capRec
}

type capRec struct {
	msg   string
	attrs map[string]any
}

func (l *capLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *capLog) Handle(_ context.Context, r slog.Record) error {
	rec := capRec{msg: r.Message, attrs: map[string]any{}}
	r.Attrs(func(a slog.Attr) bool { rec.attrs[a.Key] = a.Value.Any(); return true })
	l.mu.Lock()
	l.recs = append(l.recs, rec)
	l.mu.Unlock()
	return nil
}
func (l *capLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *capLog) WithGroup(string) slog.Handler      { return l }

func (l *capLog) hasMsg(substr string) (capRec, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.recs {
		if contains(r.msg, substr) {
			return r, true
		}
	}
	return capRec{}, false
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// The readiness wait must COVER a cold worker's pull+load: base for an
// unknown-size model, base + bytes/rate for a known one, capped at the max —
// and a max set below the base never cuts the bound.
func TestReadinessTimeoutFor_CoversPullAndLoad(t *testing.T) {
	newMgr := func() *Manager {
		return NewManager(ManagerConfig{
			Self: ShardPeer{Host: "leader", PeerID: "L"}, Peers: &fakePeers{},
			ReadinessBase: 3 * time.Minute,
			ReadinessMax:  10 * time.Minute,
			ModelBytes: func(model string) (uint64, bool) {
				switch model {
				case "tiny":
					return 100 << 20, true // 100 MiB → ~20s extra
				case "big":
					return 40 << 30, true // 40 GiB → capped
				default:
					return 0, false
				}
			},
		})
	}
	m := newMgr()
	if got := m.readinessTimeoutFor("unknown-model"); got != 3*time.Minute {
		t.Errorf("unknown size → base only, got %s", got)
	}
	if got := m.readinessTimeoutFor("tiny"); got != 3*time.Minute+20*time.Second {
		t.Errorf("100MiB @ 5MiB/s → base+20s, got %s", got)
	}
	if got := m.readinessTimeoutFor("big"); got != 10*time.Minute {
		t.Errorf("40GiB must cap at the max, got %s", got)
	}

	// A max below the base must not cut the bound: the pull still needs it.
	m2 := NewManager(ManagerConfig{
		Self: ShardPeer{Host: "leader", PeerID: "L"}, Peers: &fakePeers{},
		ReadinessBase: time.Minute, ReadinessMax: 30 * time.Second,
		ModelBytes: func(string) (uint64, bool) { return 1 << 30, true },
	})
	want := time.Minute + time.Duration((1<<30)/minPullLoadBytesPerSec)*time.Second
	if got := m2.readinessTimeoutFor("any"); got != want {
		t.Errorf("max < base must not cut the bound: got %s, want %s", got, want)
	}
}

// A ring that keeps making pull/load progress must NOT die at the base
// deadline: new progress re-grants the window and is logged.
func TestMonitorRing_ProgressExtendsWait(t *testing.T) {
	logs := &capLog{}
	m := NewManager(ManagerConfig{
		Self: ShardPeer{Host: "leader", PeerID: "L"}, Peers: &fakePeers{},
		Logger:        slog.New(logs),
		ReadinessBase: 200 * time.Millisecond,
		ReadinessMax:  5 * time.Second,
		ReadinessPoll: 10 * time.Millisecond,
	})
	readyAt := time.Now().Add(600 * time.Millisecond) // well past the 200ms base
	m.healthy = func(string) bool { return time.Now().After(readyAt) }
	var mu sync.Mutex
	n := 0
	m.progress = func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		// New text every poll: a pull that keeps advancing.
		return "pull chunk " + strings.Repeat("x", n)
	}

	ring := ring2()
	start := time.Now()
	done := make(chan struct{})
	go func() { m.monitorRing(&ring, "big-model", 11434); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("monitorRing never returned despite the endpoint going healthy")
	}
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Errorf("returned after %s — before the endpoint was healthy (no extension?)", elapsed)
	}
	if rec, ok := logs.hasMsg("wait extended"); !ok {
		t.Error("no progress-extension log line; a slow pull must report progress, not wait silently")
	} else if _, ok := rec.attrs["progress"]; !ok {
		t.Error("extension log carries no progress text")
	}
}

// A ring with NO progress anywhere must still tear down — but the timeout
// must say what it was waiting on (last observed progress), not die silent.
func TestMonitorRing_StallTearsDownAndReportsProgress(t *testing.T) {
	logs := &capLog{}
	f := newFake()
	m := NewManager(ManagerConfig{
		Self: ShardPeer{Host: "leader", PeerID: "L"}, Forwarder: f, Peers: &fakePeers{},
		Logger:        slog.New(logs),
		ReadinessBase: 150 * time.Millisecond,
		ReadinessMax:  300 * time.Millisecond,
		ReadinessPoll: 10 * time.Millisecond,
	})
	m.healthy = func(string) bool { return false }
	m.progress = func() string { return "stuck at layer 3/80" }

	// A live leader session to observe the teardown through.
	plan, err := ring2().PlanFor(0)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := Start(context.Background(), f, plan, LaunchConfig{
		BinaryPath: writeStub(t), ModelPath: "/m.gguf",
	})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.active = sess
	m.activeModel = "stalled-model"
	m.mu.Unlock()

	ring := Ring{Members: []Member{{Rank: 0, Host: "leader", PeerID: "L"}}} // leader only: no mesh dials
	start := time.Now()
	done := make(chan struct{})
	go func() { m.monitorRing(&ring, "stalled-model", 11434); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("monitorRing never tore the stalled shard down")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("stalled ring took %s to tear down (base 150ms, max 300ms)", elapsed)
	}
	if sess.Running() {
		t.Error("teardown left the leader prima process running")
	}
	if m.ActiveModel() != "" {
		t.Errorf("ActiveModel = %q after teardown, want cleared", m.ActiveModel())
	}
	rec, ok := logs.hasMsg("tearing down stuck shard")
	if !ok {
		t.Fatal("no teardown log line")
	}
	if got, _ := rec.attrs["last_progress"].(string); !contains(got, "stuck at layer 3/80") {
		t.Errorf("teardown must report the last progress, got %q", got)
	}
}
