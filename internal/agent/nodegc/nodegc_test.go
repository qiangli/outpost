package nodegc

import (
	"context"
	"errors"
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

func collector(cs *fake.Clientset) *Collector {
	return &Collector{Client: cs, Now: func() time.Time { return tickNow }}
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
	cs := fake.NewSimpleClientset(
		staleAgentNode("pi-newest", "pi", "u1", 25*time.Hour),
		staleAgentNode("pi-oldest", "pi", "u2", 100*time.Hour),
		staleAgentNode("pi-mid", "pi", "u3", 50*time.Hour),
		staleAgentNode("pi-fourth", "pi", "u4", 30*time.Hour),
		// Never candidates, must survive any pass:
		node("pi-ready", "u5", withLabels(agentLabels("pi")), withReady(corev1.ConditionTrue, tickNow.Add(-100*time.Hour))),
		node("laptop-foreign", "u6", withReady(corev1.ConditionFalse, tickNow.Add(-100*time.Hour))),
	)
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
	cs := fake.NewSimpleClientset(
		staleAgentNode("pi-charlie", "pi", "u1", ts),
		staleAgentNode("pi-alpha", "pi", "u2", ts),
		staleAgentNode("pi-bravo", "pi", "u3", ts),
		staleAgentNode("pi-delta", "pi", "u4", ts),
	)
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
	cs := fake.NewSimpleClientset(
		staleAgentNode("pi-a", "pi", "u1", 100*time.Hour),
		staleAgentNode("pi-b", "pi", "u2", 50*time.Hour),
	)
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
	cs := fake.NewSimpleClientset(
		staleAgentNode("pi-a", "pi", "u1", 100*time.Hour),
		staleAgentNode("pi-b", "pi", "u2", 50*time.Hour),
	)
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
	cs := fake.NewSimpleClientset(
		staleAgentNode("pi-a", "pi", "u1", 3*time.Hour),
		staleAgentNode("pi-b", "pi", "u2", 2*time.Hour),
		staleAgentNode("pi-c", "pi", "u3", 90*time.Minute),
	)
	c := &Collector{
		Client:     cs,
		Grace:      time.Hour,
		MaxDeletes: 1,
		Now:        func() time.Time { return tickNow },
	}
	if err := c.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 1 || got[0] != "pi-a" {
		t.Fatalf("deleted %v, want [pi-a] (custom budget 1, oldest first)", got)
	}
}
