package nodegc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// tickNow is the fixed injected clock every test measures staleness
// against. Chosen arbitrary-but-real so RFC3339 log output stays sane.
var tickNow = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

type nodeOpt func(*corev1.Node)

func withLabels(l map[string]string) nodeOpt {
	return func(n *corev1.Node) { n.Labels = l }
}

func withLabel(k, v string) nodeOpt {
	return func(n *corev1.Node) {
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		n.Labels[k] = v
	}
}

func agentLabels(host string) map[string]string {
	return map[string]string{
		RuntimeLabel: RuntimeAgent,
		BackendLabel: BackendK3s,
		HostLabel:    host,
	}
}

func withReady(status corev1.ConditionStatus, transition time.Time) nodeOpt {
	return func(n *corev1.Node) {
		n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{
			Type:               corev1.NodeReady,
			Status:             status,
			LastTransitionTime: metav1.NewTime(transition),
		})
	}
}

func withCondition(cond corev1.NodeCondition) nodeOpt {
	return func(n *corev1.Node) {
		n.Status.Conditions = append(n.Status.Conditions, cond)
	}
}

func node(name string, uid types.UID, opts ...nodeOpt) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}}
	for _, o := range opts {
		o(n)
	}
	return n
}

// staleAgentNode is the canonical GC candidate: correct labels, matching
// name prefix, NotReady for longer than the default grace.
func staleAgentNode(name, host string, uid types.UID, notReadyFor time.Duration) *corev1.Node {
	return node(name, uid,
		withLabels(agentLabels(host)),
		withReady(corev1.ConditionFalse, tickNow.Add(-notReadyFor)),
	)
}

// readyAgentNode is an in-scope, healthy fleet member. Tests add these
// so the mass-stale circuit breaker sees a mostly-healthy population —
// exactly the state the per-node deletion logic is licensed to act in.
func readyAgentNode(name, host string, uid types.UID) *corev1.Node {
	return node(name, uid,
		withLabels(agentLabels(host)),
		withReady(corev1.ConditionTrue, tickNow.Add(-time.Hour)),
	)
}

func readyFleet(host string, n int) []runtime.Object {
	var out []runtime.Object
	for i := 0; i < n; i++ {
		name := host + "-ok" + string(rune('a'+i))
		out = append(out, readyAgentNode(name, host, types.UID("ready-"+name)))
	}
	return out
}

// collector returns a Collector primed the way a long-settled healthy
// daemon would be: fixed test clock, and monotonic uptime well past
// the deletion gate. Tests for the gates themselves construct their
// own Collector with hostile values.
func collector(cs *fake.Clientset) *Collector {
	return &Collector{
		Client: cs,
		Now:    func() time.Time { return tickNow },
		Uptime: func() time.Duration { return 2 * DefaultMinUptime },
	}
}

func deletedNames(cs *fake.Clientset) []string {
	var names []string
	for _, a := range cs.Actions() {
		if del, ok := a.(k8stesting.DeleteActionImpl); ok && a.GetResource().Resource == "nodes" {
			names = append(names, del.GetName())
		}
	}
	return names
}

func actionCount(cs *fake.Clientset, verb string) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == "nodes" {
			n++
		}
	}
	return n
}

func tmpLedger(t *testing.T) *Ledger {
	t.Helper()
	return NewLedger(filepath.Join(t.TempDir(), "nodegc.log"))
}

func ledgerEvents(t *testing.T, l *Ledger) []string {
	t.Helper()
	entries, err := l.Tail(1000)
	if err != nil {
		t.Fatalf("ledger tail: %v", err)
	}
	var events []string
	for _, e := range entries {
		events = append(events, e.Event)
	}
	return events
}

func TestStaleSince(t *testing.T) {
	old := tickNow.Add(-25 * time.Hour)
	cases := []struct {
		name string
		node *corev1.Node
		want bool
	}{
		{"nil node", nil, false},
		{"canonical stale NotReady", staleAgentNode("pi-abc123", "pi", "u1", 25*time.Hour), true},
		{"stale Unknown", node("pi-abc123", "u1", withLabels(agentLabels("pi")), withReady(corev1.ConditionUnknown, old)), true},
		{"Ready node", node("pi-abc123", "u1", withLabels(agentLabels("pi")), withReady(corev1.ConditionTrue, old)), false},
		{"no labels at all", node("pi-abc123", "u1", withReady(corev1.ConditionFalse, old)), false},
		{"virtual runtime", node("pi-vk-ollama", "u1",
			withLabels(map[string]string{RuntimeLabel: "virtual", BackendLabel: "vk-ollama", HostLabel: "pi"}),
			withReady(corev1.ConditionFalse, old)), false},
		{"wrong backend", node("pi-abc123", "u1",
			withLabels(map[string]string{RuntimeLabel: RuntimeAgent, BackendLabel: "k0s", HostLabel: "pi"}),
			withReady(corev1.ConditionFalse, old)), false},
		{"missing host label", node("pi-abc123", "u1",
			withLabels(map[string]string{RuntimeLabel: RuntimeAgent, BackendLabel: BackendK3s}),
			withReady(corev1.ConditionFalse, old)), false},
		{"empty host label", node("pi-abc123", "u1",
			withLabels(map[string]string{RuntimeLabel: RuntimeAgent, BackendLabel: BackendK3s, HostLabel: ""}),
			withReady(corev1.ConditionFalse, old)), false},
		{"name not prefixed by host", node("laptop-abc123", "u1", withLabels(agentLabels("pi")), withReady(corev1.ConditionFalse, old)), false},
		{"name equals host without suffix", node("pi", "u1", withLabels(agentLabels("pi")), withReady(corev1.ConditionFalse, old)), false},
		{"host prefix without dash separator", node("pizza-abc123", "u1", withLabels(agentLabels("pi")), withReady(corev1.ConditionFalse, old)), false},
		{"NotReady exactly at grace boundary", staleAgentNode("pi-abc123", "pi", "u1", DefaultGrace), false},
		{"NotReady just past grace", staleAgentNode("pi-abc123", "pi", "u1", DefaultGrace+time.Second), true},
		{"NotReady recently", staleAgentNode("pi-abc123", "pi", "u1", time.Hour), false},
		{"missing Ready condition", node("pi-abc123", "u1", withLabels(agentLabels("pi")),
			withCondition(corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(old)})), false},
		{"zero LastTransitionTime", node("pi-abc123", "u1", withLabels(agentLabels("pi")),
			withCondition(corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse})), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			since, got := StaleSince(tc.node, DefaultGrace, tickNow)
			if got != tc.want {
				t.Fatalf("StaleSince = %v, want %v", got, tc.want)
			}
			if got && since.IsZero() {
				t.Fatal("stale node reported zero since-time")
			}
			if !got && !since.IsZero() {
				t.Fatalf("non-candidate reported since = %v, want zero", since)
			}
		})
	}
}

func TestOnceDeletesOldestFirstBounded(t *testing.T) {
	objs := append(readyFleet("pi", 5),
		staleAgentNode("pi-newest", "pi", "u1", 25*time.Hour),
		staleAgentNode("pi-oldest", "pi", "u2", 100*time.Hour),
		staleAgentNode("pi-mid", "pi", "u3", 50*time.Hour),
		staleAgentNode("pi-fourth", "pi", "u4", 30*time.Hour),
		// Never candidates, must survive any pass:
		node("pi-ready", "u5", withLabels(agentLabels("pi")), withReady(corev1.ConditionTrue, tickNow.Add(-100*time.Hour))),
		node("laptop-foreign", "u6", withReady(corev1.ConditionFalse, tickNow.Add(-100*time.Hour))),
	)
	cs := fake.NewSimpleClientset(objs...)
	if err := collector(cs).Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	got := deletedNames(cs)
	want := []string{"pi-oldest", "pi-mid", "pi-fourth"}
	if len(got) != len(want) {
		t.Fatalf("deleted %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("deleted %v, want %v (oldest first, max %d)", got, want, DefaultMaxDeletes)
		}
	}
	// The newest stale node waits for a later tick; non-candidates persist.
	for _, survivor := range []string{"pi-newest", "pi-ready", "laptop-foreign"} {
		if _, err := cs.CoreV1().Nodes().Get(context.Background(), survivor, metav1.GetOptions{}); err != nil {
			t.Fatalf("survivor %s gone: %v", survivor, err)
		}
	}
}

func TestOnceEqualTimestampsOrderByName(t *testing.T) {
	ts := 48 * time.Hour
	objs := append(readyFleet("pi", 5),
		staleAgentNode("pi-charlie", "pi", "u1", ts),
		staleAgentNode("pi-alpha", "pi", "u2", ts),
		staleAgentNode("pi-bravo", "pi", "u3", ts),
		staleAgentNode("pi-delta", "pi", "u4", ts),
	)
	cs := fake.NewSimpleClientset(objs...)
	if err := collector(cs).Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	got := deletedNames(cs)
	want := []string{"pi-alpha", "pi-bravo", "pi-charlie"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("deleted %v, want %v", got, want)
	}
}

func TestOnceDeleteCarriesUIDPrecondition(t *testing.T) {
	cs := fake.NewSimpleClientset(staleAgentNode("pi-dead", "pi", "uid-original", 48*time.Hour))
	var gotUID *types.UID
	cs.PrependReactor("delete", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
		del := a.(k8stesting.DeleteActionImpl)
		gotUID = del.DeleteOptions.Preconditions.UID
		return false, nil, nil // fall through to the tracker
	})
	if err := collector(cs).Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if gotUID == nil || *gotUID != types.UID("uid-original") {
		t.Fatalf("delete precondition UID = %v, want uid-original", gotUID)
	}
}

func TestOnceSkipsNodeRecoveredSinceList(t *testing.T) {
	stale := staleAgentNode("pi-flappy", "pi", "u1", 48*time.Hour)
	cs := fake.NewSimpleClientset(stale)
	// Between the list and the pre-delete re-GET, the node went Ready.
	recovered := node("pi-flappy", "u1", withLabels(agentLabels("pi")), withReady(corev1.ConditionTrue, tickNow.Add(-time.Minute)))
	cs.PrependReactor("get", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, recovered, nil
	})
	if err := collector(cs).Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("deleted %v, want none — node recovered before delete", got)
	}
}

func TestOnceSkipsReplacementUID(t *testing.T) {
	cs := fake.NewSimpleClientset(staleAgentNode("pi-abc123", "pi", "uid-old", 48*time.Hour))
	// Same name, different UID: a rejoin replaced the object after the
	// list snapshot. Even if the replacement LOOKS stale (a carried-over
	// condition), the UID mismatch alone must protect it.
	replacement := staleAgentNode("pi-abc123", "pi", "uid-new", 48*time.Hour)
	cs.PrependReactor("get", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, replacement, nil
	})
	if err := collector(cs).Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("deleted %v, want none — replacement UID must survive", got)
	}
}

func TestOnceStopsOnListError(t *testing.T) {
	cs := fake.NewSimpleClientset(staleAgentNode("pi-dead", "pi", "u1", 48*time.Hour))
	boom := errors.New("apiserver down")
	cs.PrependReactor("list", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})
	err := collector(cs).Once(context.Background())
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("Once err = %v, want wrapped %v", err, boom)
	}
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("deleted %v after list error, want none", got)
	}
}

func TestOnceStopsOnGetError(t *testing.T) {
	objs := append(readyFleet("pi", 3),
		staleAgentNode("pi-a", "pi", "u1", 100*time.Hour),
		staleAgentNode("pi-b", "pi", "u2", 50*time.Hour),
	)
	cs := fake.NewSimpleClientset(objs...)
	boom := errors.New("get exploded")
	cs.PrependReactor("get", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})
	err := collector(cs).Once(context.Background())
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("Once err = %v, want wrapped %v", err, boom)
	}
	// The first candidate's re-GET failed; the pass must stop before ANY
	// delete, including the second candidate's.
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("deleted %v after get error, want none", got)
	}
}

func TestOnceStopsOnDeleteError(t *testing.T) {
	objs := append(readyFleet("pi", 3),
		staleAgentNode("pi-a", "pi", "u1", 100*time.Hour),
		staleAgentNode("pi-b", "pi", "u2", 50*time.Hour),
	)
	cs := fake.NewSimpleClientset(objs...)
	boom := errors.New("delete refused")
	cs.PrependReactor("delete", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})
	err := collector(cs).Once(context.Background())
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("Once err = %v, want wrapped %v", err, boom)
	}
	// Exactly one delete was ATTEMPTED (the oldest); its failure stopped
	// the pass before pi-b was touched.
	if got := deletedNames(cs); len(got) != 1 || got[0] != "pi-a" {
		t.Fatalf("delete attempts = %v, want [pi-a] only", got)
	}
	if _, err := cs.CoreV1().Nodes().Get(context.Background(), "pi-b", metav1.GetOptions{}); err != nil {
		t.Fatalf("pi-b should be untouched: %v", err)
	}
}

func TestOnceRequiresClient(t *testing.T) {
	c := &Collector{}
	if err := c.Once(context.Background()); err == nil {
		t.Fatal("Once with nil Client should error")
	}
}

func TestOnceCustomGraceAndBudget(t *testing.T) {
	objs := append(readyFleet("pi", 4),
		staleAgentNode("pi-a", "pi", "u1", 3*time.Hour),
		staleAgentNode("pi-b", "pi", "u2", 2*time.Hour),
		staleAgentNode("pi-c", "pi", "u3", 90*time.Minute),
	)
	cs := fake.NewSimpleClientset(objs...)
	c := &Collector{
		Client:     cs,
		Grace:      time.Hour,
		MaxDeletes: 1,
		Now:        func() time.Time { return tickNow },
		Uptime:     func() time.Duration { return 2 * DefaultMinUptime },
	}
	if err := c.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 1 || got[0] != "pi-a" {
		t.Fatalf("deleted %v, want [pi-a] (custom budget 1, oldest first)", got)
	}
}

// --- Fix 1: cluster-wide unhealthy circuit breaker ---------------------

// TestOnceMassPartitionRefusesPass is the mandatory mass-stale test: a
// >grace partition flips EVERY worker to Unknown with the same
// LastTransitionTime. The pass must refuse outright — not drain the
// cluster MaxDeletes at a time — and leave durable evidence.
func TestOnceMassPartitionRefusesPass(t *testing.T) {
	partitionAt := tickNow.Add(-30 * time.Hour) // one instant, fleet-wide
	var objs []runtime.Object
	for _, s := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		objs = append(objs, node("pi-"+s, types.UID("u-"+s),
			withLabels(agentLabels("pi")),
			withReady(corev1.ConditionUnknown, partitionAt)))
	}
	cs := fake.NewSimpleClientset(objs...)
	c := collector(cs)
	c.Ledger = tmpLedger(t)
	if err := c.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("deleted %v under mass partition, want NONE — the observer is the suspect", got)
	}
	events := ledgerEvents(t, c.Ledger)
	if len(events) != 1 || events[0] != EventRefusedMassStale {
		t.Fatalf("ledger events = %v, want [%s]", events, EventRefusedMassStale)
	}
	// The refusal must repeat, not decay into deletion on later ticks.
	if err := c.Once(context.Background()); err != nil {
		t.Fatalf("second Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("second pass deleted %v, want none", got)
	}
}

func TestOnceStaleFractionThreshold(t *testing.T) {
	// 5 stale of 10 in scope is exactly the default 0.5 — NOT refused
	// (strictly-greater trips the breaker); 6 of 10 is.
	mk := func(staleN, readyN int) *fake.Clientset {
		objs := readyFleet("pi", readyN)
		for i := 0; i < staleN; i++ {
			name := "pi-dead" + string(rune('a'+i))
			objs = append(objs, staleAgentNode(name, "pi", types.UID(name), 48*time.Hour))
		}
		return fake.NewSimpleClientset(objs...)
	}

	cs := mk(5, 5)
	if err := collector(cs).Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != DefaultMaxDeletes {
		t.Fatalf("at-threshold pass deleted %v, want %d deletions", got, DefaultMaxDeletes)
	}

	cs = mk(6, 4)
	if err := collector(cs).Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("above-threshold pass deleted %v, want none", got)
	}
}

// TestOnceSingleStaleNodeBypassesBreaker: one stale node is bounded
// blast radius by definition — a one-worker home cluster must still be
// able to reap its single decommissioned ghost (1/1 is 100% stale).
func TestOnceSingleStaleNodeBypassesBreaker(t *testing.T) {
	cs := fake.NewSimpleClientset(staleAgentNode("pi-gone", "pi", "u1", 48*time.Hour))
	if err := collector(cs).Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 1 || got[0] != "pi-gone" {
		t.Fatalf("deleted %v, want [pi-gone]", got)
	}
}

// --- Fix 2: settle window + restart-safe rate budget --------------------

// TestOnceBudgetSurvivesRestart: the delete budget is anchored to wall
// clock via the ledger, so a restarted collector (fresh memory, same
// ledger) gets NO fresh budget inside the window — and gets it back
// once the window has actually elapsed.
func TestOnceBudgetSurvivesRestart(t *testing.T) {
	objs := readyFleet("pi", 6)
	stales := []struct {
		name string
		age  time.Duration
	}{
		{"pi-s1", 100 * time.Hour}, {"pi-s2", 90 * time.Hour}, {"pi-s3", 80 * time.Hour},
		{"pi-s4", 70 * time.Hour}, {"pi-s5", 60 * time.Hour},
	}
	for _, s := range stales {
		objs = append(objs, staleAgentNode(s.name, "pi", types.UID(s.name), s.age))
	}
	cs := fake.NewSimpleClientset(objs...)
	ledger := tmpLedger(t)

	c1 := collector(cs)
	c1.Ledger = ledger
	if err := c1.Once(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if got := deletedNames(cs); len(got) != DefaultMaxDeletes {
		t.Fatalf("first pass deleted %v, want %d", got, DefaultMaxDeletes)
	}

	// "Restart": a brand-new Collector with empty memory but the same
	// ledger, still inside the rate window. Before the fix this minted a
	// fresh MaxDeletes budget per restart (~10 toggles ≈ 30 deletions).
	c2 := collector(cs)
	c2.Ledger = ledger
	if err := c2.Once(context.Background()); err != nil {
		t.Fatalf("post-restart pass: %v", err)
	}
	if got := deletedNames(cs); len(got) != DefaultMaxDeletes {
		t.Fatalf("post-restart pass deleted more: %v — budget must survive restarts", got)
	}

	// Once the wall-clock window has elapsed, deletion resumes.
	later := tickNow.Add(2 * DefaultInterval)
	c3 := collector(cs)
	c3.Ledger = ledger
	c3.Now = func() time.Time { return later }
	if err := c3.Once(context.Background()); err != nil {
		t.Fatalf("post-window pass: %v", err)
	}
	if got := deletedNames(cs); len(got) != 5 {
		t.Fatalf("post-window total deletions = %v, want all 5 stale nodes reaped", got)
	}
}

func TestOnceLedgerReadErrorRefusesDeletes(t *testing.T) {
	cs := fake.NewSimpleClientset(staleAgentNode("pi-dead", "pi", "u1", 48*time.Hour))
	c := collector(cs)
	// A ledger whose path is a DIRECTORY: Append and the budget read
	// both fail. Destructive code must fail safe — no deletes.
	c.Ledger = NewLedger(t.TempDir())
	if err := c.Once(context.Background()); err == nil {
		t.Fatal("Once with unreadable ledger should surface an error")
	}
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("deleted %v with unreadable ledger, want none (refuse to delete blind)", got)
	}
}

// TestRunWaitsSettleWindow: Run must NOT pass immediately on start —
// outpost restarts on every builtin toggle, and an immediate pass per
// restart is what made the old per-pass budget multiply.
func TestRunWaitsSettleWindow(t *testing.T) {
	cs := fake.NewSimpleClientset(staleAgentNode("pi-dead", "pi", "u1", 48*time.Hour))
	c := collector(cs)
	c.Settle = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done
	if n := actionCount(cs, "list"); n != 0 {
		t.Fatalf("Run performed %d list(s) inside the settle window, want 0", n)
	}

	// And after the settle window elapses, the first pass does run.
	c2 := collector(cs)
	c2.Settle = 20 * time.Millisecond
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { c2.Run(ctx2); close(done2) }()
	deadline := time.Now().Add(3 * time.Second)
	for actionCount(cs, "list") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel2()
	<-done2
	if n := actionCount(cs, "list"); n == 0 {
		t.Fatal("Run never passed after the settle window elapsed")
	}
}

// --- Fix 3: clock-skew guards -------------------------------------------

func TestOnceFutureTimestampRefusesPass(t *testing.T) {
	cs := fake.NewSimpleClientset(
		staleAgentNode("pi-dead", "pi", "u1", 48*time.Hour),
		// Evidence recorded in OUR future, beyond tolerance: the clocks
		// provably disagree, so no verdict this pass can be trusted.
		node("pi-fromfuture", "u2", withLabels(agentLabels("pi")),
			withReady(corev1.ConditionTrue, tickNow.Add(time.Hour))),
	)
	c := collector(cs)
	c.Ledger = tmpLedger(t)
	if err := c.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("deleted %v with future-stamped evidence in scope, want none", got)
	}
	events := ledgerEvents(t, c.Ledger)
	if len(events) != 1 || events[0] != EventRefusedClockSkew {
		t.Fatalf("ledger events = %v, want [%s]", events, EventRefusedClockSkew)
	}
}

func TestOnceFutureWithinToleranceProceeds(t *testing.T) {
	cs := fake.NewSimpleClientset(
		staleAgentNode("pi-dead", "pi", "u1", 48*time.Hour),
		// Two minutes ahead is ordinary NTP drift, not a broken RTC.
		node("pi-drift", "u2", withLabels(agentLabels("pi")),
			withReady(corev1.ConditionTrue, tickNow.Add(2*time.Minute))),
	)
	if err := collector(cs).Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 1 || got[0] != "pi-dead" {
		t.Fatalf("deleted %v, want [pi-dead]", got)
	}
}

// TestOnceUptimeGateBlocksDeletes: a freshly-booted collector — exactly
// when a wrong RTC is likeliest — must run read-only until its own
// monotonic uptime covers a meaningful part of the grace window.
func TestOnceUptimeGateBlocksDeletes(t *testing.T) {
	cs := fake.NewSimpleClientset(staleAgentNode("pi-dead", "pi", "u1", 48*time.Hour))
	c := &Collector{
		Client: cs,
		Now:    func() time.Time { return tickNow },
		Uptime: func() time.Duration { return 2 * time.Minute },
	}
	if err := c.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("deleted %v at 2m uptime, want none (min uptime %s)", got, DefaultMinUptime)
	}

	// Same collector, uptime now past the gate: deletion proceeds.
	c.Uptime = func() time.Duration { return DefaultMinUptime + time.Minute }
	if err := c.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 1 || got[0] != "pi-dead" {
		t.Fatalf("deleted %v after uptime gate cleared, want [pi-dead]", got)
	}
}

// --- Fix 4: dry run + durable record -------------------------------------

func TestOnceDryRunDeletesNothing(t *testing.T) {
	objs := append(readyFleet("pi", 5),
		staleAgentNode("pi-s1", "pi", "u1", 100*time.Hour),
		staleAgentNode("pi-s2", "pi", "u2", 50*time.Hour),
	)
	cs := fake.NewSimpleClientset(objs...)
	ledger := tmpLedger(t)
	c := collector(cs)
	c.DryRun = true
	c.Ledger = ledger
	if err := c.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("dry run deleted %v, want none", got)
	}
	events := ledgerEvents(t, ledger)
	if len(events) != 2 || events[0] != EventDryRunDeleted || events[1] != EventDryRunDeleted {
		t.Fatalf("ledger events = %v, want two %s entries", events, EventDryRunDeleted)
	}
	// Dry-run entries must not consume the REAL budget: a real pass over
	// the same ledger still deletes.
	real := collector(cs)
	real.Ledger = ledger
	if err := real.Once(context.Background()); err != nil {
		t.Fatalf("real pass: %v", err)
	}
	if got := deletedNames(cs); len(got) != 2 {
		t.Fatalf("real pass after dry run deleted %v, want both stale nodes", got)
	}
}

func TestOnceDeletionsAreLedgered(t *testing.T) {
	cs := fake.NewSimpleClientset(staleAgentNode("pi-dead", "pi", "uid-1", 48*time.Hour))
	ledger := tmpLedger(t)
	c := collector(cs)
	c.Ledger = ledger
	if err := c.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	entries, err := ledger.Tail(10)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Event != EventDeleted || e.Node != "pi-dead" || e.UID != "uid-1" || e.NotReadySince == "" {
		t.Fatalf("ledger entry = %+v, want deleted pi-dead uid-1 with not_ready_since", e)
	}
	if !e.At.Equal(tickNow) {
		t.Fatalf("ledger At = %v, want injected clock %v", e.At, tickNow)
	}
}

// --- Fix 5: ownership / structural scope ---------------------------------

func TestOnceNeverReapsOwnHost(t *testing.T) {
	cs := fake.NewSimpleClientset(
		// The control-plane host's OWN agent node, however stale it looks.
		staleAgentNode("plane-abc123", "plane", "u1", 200*time.Hour),
		staleAgentNode("worker-def456", "worker", "u2", 48*time.Hour),
	)
	c := collector(cs)
	c.SelfHost = "plane"
	if err := c.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 1 || got[0] != "worker-def456" {
		t.Fatalf("deleted %v, want [worker-def456] only — never our own node", got)
	}
	if _, err := cs.CoreV1().Nodes().Get(context.Background(), "plane-abc123", metav1.GetOptions{}); err != nil {
		t.Fatalf("own node reaped: %v", err)
	}
}

func TestOnceNeverReapsControlPlaneRole(t *testing.T) {
	for _, role := range []string{ControlPlaneRoleLabel, MasterRoleLabel} {
		cs := fake.NewSimpleClientset(
			node("pi-plane", "u1", withLabels(agentLabels("pi")), withLabel(role, "true"),
				withReady(corev1.ConditionUnknown, tickNow.Add(-200*time.Hour))),
			staleAgentNode("pi-worker", "pi", "u2", 48*time.Hour),
		)
		if err := collector(cs).Once(context.Background()); err != nil {
			t.Fatalf("Once (%s): %v", role, err)
		}
		if got := deletedNames(cs); len(got) != 1 || got[0] != "pi-worker" {
			t.Fatalf("deleted %v with %s in play, want [pi-worker] only", got, role)
		}
	}
}

// TestOnceReGetReappliesStructuralExclusions: the pre-delete re-GET
// must re-run the FULL predicate — a node that acquired a control-plane
// role between list and delete survives.
func TestOnceReGetReappliesStructuralExclusions(t *testing.T) {
	cs := fake.NewSimpleClientset(staleAgentNode("pi-promoted", "pi", "u1", 48*time.Hour))
	promoted := staleAgentNode("pi-promoted", "pi", "u1", 48*time.Hour)
	withLabel(ControlPlaneRoleLabel, "true")(promoted)
	cs.PrependReactor("get", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, promoted, nil
	})
	if err := collector(cs).Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("deleted %v, want none — re-GET must re-apply exclusions", got)
	}
}

// --- Ledger unit coverage -------------------------------------------------

func TestLedgerRoundTrip(t *testing.T) {
	l := tmpLedger(t)
	if err := l.Append(LedgerEntry{At: tickNow, Event: EventDeleted, Node: "pi-a"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := l.Append(LedgerEntry{At: tickNow.Add(time.Minute), Event: EventDryRunDeleted, Node: "pi-b"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	n, err := l.CountDeletesSince(tickNow.Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("CountDeletesSince = %d, %v; want 1 (dry-run never counts)", n, err)
	}
	n, err = l.CountDeletesSince(tickNow) // strictly-after cutoff
	if err != nil || n != 0 {
		t.Fatalf("CountDeletesSince(at) = %d, %v; want 0", n, err)
	}
	entries, err := l.Tail(1)
	if err != nil || len(entries) != 1 || entries[0].Node != "pi-b" {
		t.Fatalf("Tail(1) = %+v, %v; want newest entry pi-b", entries, err)
	}
}

func TestLedgerMissingFileAndNil(t *testing.T) {
	l := NewLedger(filepath.Join(t.TempDir(), "never-written.log"))
	if n, err := l.CountDeletesSince(time.Time{}); err != nil || n != 0 {
		t.Fatalf("missing file: %d, %v; want 0, nil", n, err)
	}
	var nilLedger *Ledger
	if err := nilLedger.Append(LedgerEntry{Event: EventDeleted}); err != nil {
		t.Fatalf("nil Append: %v", err)
	}
	if n, err := nilLedger.CountDeletesSince(time.Time{}); err != nil || n != 0 {
		t.Fatalf("nil count: %d, %v", n, err)
	}
}

func TestLedgerSkipsMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodegc.log")
	l := NewLedger(path)
	if err := l.Append(LedgerEntry{At: tickNow, Event: EventDeleted, Node: "pi-a"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString("not json at all\n"); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	f.Close()
	if err := l.Append(LedgerEntry{At: tickNow, Event: EventDeleted, Node: "pi-b"}); err != nil {
		t.Fatalf("append after garbage: %v", err)
	}
	n, err := l.CountDeletesSince(tickNow.Add(-time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("count with garbage line = %d, %v; want 2", n, err)
	}
}
