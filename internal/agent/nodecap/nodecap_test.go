package nodecap

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
		if a.GetVerb() == "update" && a.GetResource().Resource == "nodes" {
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
		if a.GetVerb() == "update" && a.GetResource().Resource == "nodes" {
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

func TestConstructProbeDaemonSet(t *testing.T) {
	ds := ConstructProbeDaemonSet("")
	if ds.Namespace != ProbeNamespace {
		t.Errorf("namespace = %s, want %s", ds.Namespace, ProbeNamespace)
	}
	if ds.Name != ProbeDaemonSetName {
		t.Errorf("name = %s, want %s", ds.Name, ProbeDaemonSetName)
	}
	if ds.Spec.Selector.MatchLabels["app.kubernetes.io/name"] != "dks-runtime-probe" {
		t.Errorf("selector = %v, want app.kubernetes.io/name=dks-runtime-probe", ds.Spec.Selector.MatchLabels)
	}
	if ds.Spec.Template.Labels["app.kubernetes.io/name"] != "dks-runtime-probe" {
		t.Errorf("pod labels = %v", ds.Spec.Template.Labels)
	}

	container := ds.Spec.Template.Spec.Containers[0]
	if container.Image != DefaultProbeImage {
		t.Errorf("image = %s, want %s", container.Image, DefaultProbeImage)
	}

	// Least privilege checks
	sc := container.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("expected RunAsNonRoot = true")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("expected AllowPrivilegeEscalation = false")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("expected ReadOnlyRootFilesystem = true")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) == 0 || sc.Capabilities.Drop[0] != "ALL" {
		t.Error("expected capabilities drop ALL")
	}

	// Toleration check
	hasUnavailableTol := false
	for _, tol := range ds.Spec.Template.Spec.Tolerations {
		if tol.Key == UnavailableTaint {
			hasUnavailableTol = true
			break
		}
	}
	if !hasUnavailableTol {
		t.Errorf("daemonset missing toleration for %s", UnavailableTaint)
	}

	// Virtual node exclusion via NodeAffinity
	affinity := ds.Spec.Template.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil {
		t.Fatal("expected NodeAffinity set")
	}
	terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	foundVirtualExclusion := false
	for _, term := range terms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == RuntimeLabel && expr.Operator == corev1.NodeSelectorOpNotIn {
				for _, val := range expr.Values {
					if val == RuntimeVirtual {
						foundVirtualExclusion = true
					}
				}
			}
		}
	}
	if !foundVirtualExclusion {
		t.Errorf("node affinity missing virtual node exclusion for key %s val %s", RuntimeLabel, RuntimeVirtual)
	}

	// Bounded rollout check
	if ds.Spec.UpdateStrategy.Type != appsv1.RollingUpdateDaemonSetStrategyType {
		t.Errorf("update strategy = %s, want RollingUpdate", ds.Spec.UpdateStrategy.Type)
	}
}

func TestEnsureProbeDaemonSet_Lifecycle(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()

	// Initial creation
	if err := EnsureProbeDaemonSet(ctx, cs, ""); err != nil {
		t.Fatalf("EnsureProbeDaemonSet create: %v", err)
	}
	ds, err := cs.AppsV1().DaemonSets(ProbeNamespace).Get(ctx, ProbeDaemonSetName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get daemonset: %v", err)
	}
	if ds.Spec.Template.Spec.Containers[0].Image != DefaultProbeImage {
		t.Errorf("image = %s, want %s", ds.Spec.Template.Spec.Containers[0].Image, DefaultProbeImage)
	}

	// Idempotent second call
	if err := EnsureProbeDaemonSet(ctx, cs, ""); err != nil {
		t.Fatalf("EnsureProbeDaemonSet idempotent call: %v", err)
	}

	// Update image
	newImg := "rancher/mirrored-pause:3.9"
	if err := EnsureProbeDaemonSet(ctx, cs, newImg); err != nil {
		t.Fatalf("EnsureProbeDaemonSet update image: %v", err)
	}
	ds, err = cs.AppsV1().DaemonSets(ProbeNamespace).Get(ctx, ProbeDaemonSetName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get daemonset after update: %v", err)
	}
	if ds.Spec.Template.Spec.Containers[0].Image != newImg {
		t.Errorf("updated image = %s, want %s", ds.Spec.Template.Spec.Containers[0].Image, newImg)
	}

	// Cleanup / Delete
	if err := DeleteProbeDaemonSet(ctx, cs); err != nil {
		t.Fatalf("DeleteProbeDaemonSet: %v", err)
	}
	_, err = cs.AppsV1().DaemonSets(ProbeNamespace).Get(ctx, ProbeDaemonSetName, metav1.GetOptions{})
	if err == nil {
		t.Fatal("expected daemonset to be deleted")
	}

	// Idempotent delete
	if err := DeleteProbeDaemonSet(ctx, cs); err != nil {
		t.Fatalf("DeleteProbeDaemonSet second call: %v", err)
	}
}

func TestReconcile_DisableProbeDaemonSet(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()

	// Ensure it exists first
	if err := EnsureProbeDaemonSet(ctx, cs, ""); err != nil {
		t.Fatalf("EnsureProbeDaemonSet: %v", err)
	}

	r := &Reconciler{
		Client:                cs,
		DisableProbeDaemonSet: true,
	}
	if err := r.Once(ctx); err != nil {
		t.Fatalf("Once with DisableProbeDaemonSet: %v", err)
	}

	_, err := cs.AppsV1().DaemonSets(ProbeNamespace).Get(ctx, ProbeDaemonSetName, metav1.GetOptions{})
	if err == nil {
		t.Fatal("expected daemonset to be deleted when DisableProbeDaemonSet is set")
	}
}

func TestVocabularyParity(t *testing.T) {
	if ProbeLabelSelector != "app.kubernetes.io/name=dks-runtime-probe" {
		t.Errorf("ProbeLabelSelector = %q", ProbeLabelSelector)
	}
	if ProbeNamespace != "kube-system" {
		t.Errorf("ProbeNamespace = %q", ProbeNamespace)
	}
	if ProbeDaemonSetName != "dks-runtime-probe" {
		t.Errorf("ProbeDaemonSetName = %q", ProbeDaemonSetName)
	}
	if UnavailableTaint != "outpost.dhnt.io/runtime-unavailable" {
		t.Errorf("UnavailableTaint = %q", UnavailableTaint)
	}
	if ReadyLabel != "outpost.dhnt.io/runtime-ready" {
		t.Errorf("ReadyLabel = %q", ReadyLabel)
	}
	if UnavailableReason != "outpost.dhnt.io/runtime-unavailable-reason" {
		t.Errorf("UnavailableReason = %q", UnavailableReason)
	}
	if RuntimeLabel != "outpost.dhnt.io/runtime" {
		t.Errorf("RuntimeLabel = %q", RuntimeLabel)
	}
	if RuntimeVirtual != "virtual" {
		t.Errorf("RuntimeVirtual = %q", RuntimeVirtual)
	}
}
