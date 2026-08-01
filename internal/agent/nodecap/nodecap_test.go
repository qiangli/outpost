package nodecap

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func probe(node string, ready bool, age time.Duration) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "probe-" + node,
			Namespace:         ProbeNamespace,
			Labels:            map[string]string{"app.kubernetes.io/name": "dks-runtime-probe"},
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-age)},
		},
		Spec: corev1.PodSpec{NodeName: node},
	}
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}
	return p
}

func taintKeys(n *corev1.Node) []string {
	var out []string
	for _, t := range n.Spec.Taints {
		out = append(out, t.Key)
	}
	return out
}

func hasTaint(n *corev1.Node, key string) bool {
	for _, t := range n.Spec.Taints {
		if t.Key == key {
			return true
		}
	}
	return false
}

// A Ready probe is proof the runtime can create a sandbox: label true,
// no taint.
func TestSetRuntimeCapability_ReadyMarksAvailable(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	changed, available := SetRuntimeCapability(n, probe("n1", true, time.Minute), time.Now())
	if !changed || !available {
		t.Fatalf("changed=%v available=%v, want true/true", changed, available)
	}
	if n.Labels[ReadyLabel] != "true" {
		t.Errorf("label = %q", n.Labels[ReadyLabel])
	}
	if hasTaint(n, UnavailableTaint) {
		t.Errorf("Ready node must not carry the taint: %v", taintKeys(n))
	}
}

// THE GRACE PERIOD. A probe that has not become Ready *yet* is not
// evidence of a broken runtime. Tainting here would evict workloads on
// every DaemonSet rollout.
func TestSetRuntimeCapability_NotReadyInsideGraceSaysNothing(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	changed, available := SetRuntimeCapability(n, probe("n1", false, 10*time.Second), time.Now())
	if changed || available {
		t.Fatalf("changed=%v available=%v, want false/false inside the grace window", changed, available)
	}
	if len(n.Spec.Taints) != 0 || len(n.Labels) != 0 {
		t.Error("node must be untouched inside the grace window")
	}
}

// Past the grace window a still-unready probe is real evidence: taint,
// label false, and an annotation explaining why.
func TestSetRuntimeCapability_NotReadyPastGraceTaints(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	changed, available := SetRuntimeCapability(n, probe("n1", false, ProbeGrace+time.Minute), time.Now())
	if !changed || available {
		t.Fatalf("changed=%v available=%v, want true/false", changed, available)
	}
	if !hasTaint(n, UnavailableTaint) {
		t.Errorf("expected the taint, got %v", taintKeys(n))
	}
	if n.Labels[ReadyLabel] != "false" {
		t.Errorf("label = %q", n.Labels[ReadyLabel])
	}
	if n.Annotations[UnavailableReason] == "" {
		t.Error("expected a reason annotation so `describe node` explains itself")
	}
}

// Recovery must clear both the taint and the reason — otherwise a healed
// node stays unschedulable forever.
func TestSetRuntimeCapability_RecoveryClearsTaintAndReason(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	SetRuntimeCapability(n, probe("n1", false, ProbeGrace+time.Minute), time.Now())
	if !hasTaint(n, UnavailableTaint) {
		t.Fatal("setup: expected a tainted node")
	}
	changed, available := SetRuntimeCapability(n, probe("n1", true, time.Minute), time.Now())
	if !changed || !available {
		t.Fatalf("changed=%v available=%v, want true/true", changed, available)
	}
	if hasTaint(n, UnavailableTaint) {
		t.Errorf("taint survived recovery: %v", taintKeys(n))
	}
	if _, ok := n.Annotations[UnavailableReason]; ok {
		t.Error("reason annotation survived recovery")
	}
}

// Steady state must be a no-op, or every pass issues a pointless Update.
func TestSetRuntimeCapability_IdempotentWhenAlreadyCorrect(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	SetRuntimeCapability(n, probe("n1", true, time.Minute), time.Now())
	if changed, _ := SetRuntimeCapability(n, probe("n1", true, time.Minute), time.Now()); changed {
		t.Error("second pass reported a change on an already-correct node")
	}
}

// Unrelated taints belong to other controllers and must survive.
func TestSetRuntimeCapability_PreservesOtherTaints(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	n.Spec.Taints = []corev1.Taint{{Key: "someone-else/reserved", Effect: corev1.TaintEffectNoSchedule}}
	SetRuntimeCapability(n, probe("n1", false, ProbeGrace+time.Minute), time.Now())
	SetRuntimeCapability(n, probe("n1", true, time.Minute), time.Now())
	if !hasTaint(n, "someone-else/reserved") {
		t.Errorf("clobbered another controller's taint: %v", taintKeys(n))
	}
}

// ABSENCE IS UNKNOWN, NOT FAILURE. A node with no probe yet must be left
// alone — tainting on missing evidence evicts work from healthy nodes.
func TestReconcile_NoProbeLeavesNodeAlone(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "lonely"}})
	r := &Reconciler{Client: cs}
	if err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "update" {
			t.Fatal("updated a node that has no probe — absence was treated as failure")
		}
	}
}

// Backends with no container runtime of their own (virtual kubelet) must
// be excluded by the caller's filter, not guessed at here.
func TestReconcile_FilterExcludesNodes(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "virtual", Labels: map[string]string{"kind": "virtual"}}},
		probe("virtual", false, ProbeGrace+time.Minute),
	)
	r := &Reconciler{
		Client:  cs,
		Include: func(n *corev1.Node) bool { return n.Labels["kind"] != "virtual" },
	}
	if err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "update" {
			t.Fatal("filtered node was still updated")
		}
	}
}

// A rolling update can leave two probes on one node. Any Ready probe is
// sufficient proof, even if a newer one is not Ready yet.
func TestProbesByNode_PrefersReady(t *testing.T) {
	pods := []corev1.Pod{*probe("n1", false, time.Minute), *probe("n1", true, 10*time.Second)}
	pods[1].Name = "probe-n1-new"
	got := probesByNode(pods)
	if !PodReady(got["n1"]) {
		t.Error("expected the Ready probe to win")
	}
}

// With none Ready, the OLDEST wins — so repeated replacement cannot keep
// resetting the grace period and hide a persistently broken runtime.
func TestProbesByNode_FallsBackToOldest(t *testing.T) {
	old := *probe("n1", false, 10*time.Minute)
	old.Name = "probe-old"
	fresh := *probe("n1", false, time.Second)
	fresh.Name = "probe-fresh"
	got := probesByNode([]corev1.Pod{fresh, old})
	if got["n1"].Name != "probe-old" {
		t.Errorf("got %s, want the oldest probe", got["n1"].Name)
	}
}
