package bundleapply

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// readyDeployment builds a Deployment that already reports a completed
// rollout (updated + ready + available at desired), so a wait resolves on
// the first poll.
func readyDeployment(ns, name string) *unstructured.Unstructured {
	o := newObj("apps/v1", "Deployment", ns, name)
	setNested(o, int64(1), "spec", "replicas")
	setNested(o, int64(1), "metadata", "generation")
	setNested(o, int64(1), "status", "observedGeneration")
	setNested(o, int64(1), "status", "updatedReplicas")
	setNested(o, int64(1), "status", "readyReplicas")
	setNested(o, int64(1), "status", "availableReplicas")
	return o
}

func TestApplyBundleOrderAndReady(t *testing.T) {
	fc := newFakeClient()
	objs := []*unstructured.Unstructured{
		readyDeployment("demo", "web"),
		newObj("v1", "Namespace", "", "demo"),
		newObj("v1", "ConfigMap", "demo", "cfg"),
	}
	sortByApplyRank(objs)
	b := &Bundle{Objects: objs}

	res, err := ApplyBundle(context.Background(), b, Options{
		Client:       fc,
		Timeout:      2 * time.Second,
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	if res.Applied != 3 || res.Ready != 3 {
		t.Fatalf("want applied=3 ready=3, got %+v", res)
	}
	// Namespace must be the FIRST thing applied.
	if len(fc.applyOrder) == 0 || !strings.HasPrefix(fc.applyOrder[0], "Namespace|") {
		t.Fatalf("Namespace must be applied first, order=%v", fc.applyOrder)
	}
	// A clean run created everything and rolled back nothing.
	if len(res.Created) != 3 || len(res.RolledBack) != 0 || len(res.CleanupFailed) != 0 {
		t.Fatalf("clean run accounting wrong: %+v", res)
	}
}

func TestApplyBundleIdempotent(t *testing.T) {
	fc := newFakeClient()
	b := &Bundle{Objects: []*unstructured.Unstructured{
		newObj("v1", "Namespace", "", "demo"),
		newObj("v1", "ConfigMap", "demo", "cfg"),
	}}
	opts := Options{Client: fc, Timeout: time.Second, PollInterval: 5 * time.Millisecond}

	if _, err := ApplyBundle(context.Background(), b, opts); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	res, err := ApplyBundle(context.Background(), b, opts)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	// Two runs => each object applied twice, but the store holds exactly
	// one copy of each (converged, not duplicated).
	if got := fc.applyCount["Namespace||demo"]; got != 2 {
		t.Fatalf("namespace should be applied twice across two runs, got %d", got)
	}
	if len(fc.store) != 2 {
		t.Fatalf("store must hold exactly 2 converged objects, got %d", len(fc.store))
	}
	// The second run created NOTHING — everything pre-existed, so its
	// rollback set must be empty (ownership discipline).
	if len(res.Created) != 0 {
		t.Fatalf("second run must not claim ownership of pre-existing objects: %v", res.Created)
	}
}

func TestApplyBundleReadinessTimeout(t *testing.T) {
	fc := newFakeClient()
	// A Deployment that never becomes Ready (no status populated).
	dep := newObj("apps/v1", "Deployment", "demo", "web")
	setNested(dep, int64(1), "spec", "replicas")
	setNested(dep, int64(1), "metadata", "generation")
	b := &Bundle{Objects: []*unstructured.Unstructured{dep}}

	_, err := ApplyBundle(context.Background(), b, Options{
		Client:       fc,
		Timeout:      50 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
	})
	if !errors.Is(err, ErrReadinessTimeout) {
		t.Fatalf("a never-ready workload must time out with ErrReadinessTimeout, got %v", err)
	}
}

func TestApplyBundleConvergesToReadyAcrossPolls(t *testing.T) {
	fc := newFakeClient()
	dep := newObj("apps/v1", "Deployment", "demo", "web")
	setNested(dep, int64(1), "spec", "replicas")
	setNested(dep, int64(1), "metadata", "generation")
	b := &Bundle{Objects: []*unstructured.Unstructured{dep}}

	// Flip the stored Deployment to a completed rollout on the 3rd Get.
	fc.onGet["Deployment|demo|web"] = func(call int, obj *unstructured.Unstructured) {
		if call >= 3 {
			setNested(obj, int64(1), "status", "observedGeneration")
			setNested(obj, int64(1), "status", "updatedReplicas")
			setNested(obj, int64(1), "status", "readyReplicas")
			setNested(obj, int64(1), "status", "availableReplicas")
		}
	}

	res, err := ApplyBundle(context.Background(), b, Options{
		Client:       fc,
		Timeout:      2 * time.Second,
		PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("should converge to ready: %v", err)
	}
	if res.Ready != 1 {
		t.Fatalf("want ready=1, got %d", res.Ready)
	}
}

func TestApplyBundleTerminalPodFailsFast(t *testing.T) {
	fc := newFakeClient()
	pod := newObj("v1", "Pod", "demo", "p")
	b := &Bundle{Objects: []*unstructured.Unstructured{pod}}
	// The stored pod is in phase Failed.
	fc.onGet["Pod|demo|p"] = func(_ int, obj *unstructured.Unstructured) {
		setNested(obj, "Failed", "status", "phase")
	}
	_, err := ApplyBundle(context.Background(), b, Options{
		Client:       fc,
		Timeout:      2 * time.Second,
		PollInterval: 5 * time.Millisecond,
	})
	if err == nil || errors.Is(err, ErrReadinessTimeout) {
		t.Fatalf("a Failed pod must fail fast (terminal), not time out: %v", err)
	}
}

// Evidence invariant: an unreachable apiserver during the readiness wait
// is a hard failure, never silently "ready".
func TestWaitForReadyUnreachableApiserverFails(t *testing.T) {
	fc := newFakeClient()
	ns := newObj("v1", "Namespace", "", "demo")
	if _, err := fc.Apply(context.Background(), ns); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	fc.getErr = errors.New("connection refused")
	_, err := WaitForReady(context.Background(), fc, []*unstructured.Unstructured{ns},
		WaitOptions{Timeout: time.Second, PollInterval: 5 * time.Millisecond})
	if err == nil {
		t.Fatal("unreachable apiserver must fail loudly")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error should surface the apiserver failure, got %v", err)
	}
}

func TestApplyBundleApplyErrorPropagates(t *testing.T) {
	fc := newFakeClient()
	fc.applyErr["ConfigMap|demo|cfg"] = errors.New("forbidden")
	b := &Bundle{Objects: []*unstructured.Unstructured{
		newObj("v1", "ConfigMap", "demo", "cfg"),
	}}
	_, err := ApplyBundle(context.Background(), b, Options{Client: fc, Timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("apply error must propagate, got %v", err)
	}
}

func TestApplyBundleRejectsBadOptions(t *testing.T) {
	b := &Bundle{Objects: []*unstructured.Unstructured{newObj("v1", "Namespace", "", "demo")}}
	if _, err := ApplyBundle(context.Background(), b, Options{Client: newFakeClient(), Timeout: 0}); err == nil {
		t.Fatal("zero timeout must be rejected (no unbounded wait)")
	}
	if _, err := ApplyBundle(context.Background(), b, Options{Client: nil, Timeout: time.Second}); err == nil {
		t.Fatal("nil client must be rejected")
	}
	if _, err := ApplyBundle(context.Background(), &Bundle{}, Options{Client: newFakeClient(), Timeout: time.Second}); !errors.Is(err, ErrEmptyBundle) {
		t.Fatalf("empty bundle must be ErrEmptyBundle, got %v", err)
	}
}

// --- transactional inventory / rollback (requirement 4) --------------------

// newCRD builds a CustomResourceDefinition for group example.io, kind
// Widget, served at v1.
func newCRD() *unstructured.Unstructured {
	crd := newObj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "widgets.example.io")
	setNested(crd, "example.io", "spec", "group")
	setNested(crd, "Widget", "spec", "names", "kind")
	return crd
}

// On failure, everything THIS run created is deleted again — in reverse
// apply order — while pre-existing objects are left strictly alone.
func TestApplyBundleRollsBackOnlyWhatItCreated(t *testing.T) {
	fc := newFakeClient()
	// The ConfigMap exists BEFORE the run (a prior operator action).
	pre := newObj("v1", "ConfigMap", "demo", "cfg")
	if _, err := fc.Apply(context.Background(), pre); err != nil {
		t.Fatal(err)
	}
	fc.applyOrder = nil

	dep := newObj("apps/v1", "Deployment", "demo", "web") // never becomes ready
	setNested(dep, int64(1), "spec", "replicas")
	setNested(dep, int64(1), "metadata", "generation")
	objs := []*unstructured.Unstructured{
		newObj("v1", "Namespace", "", "demo"),
		pre.DeepCopy(),
		dep,
	}
	sortByApplyRank(objs)

	res, err := ApplyBundle(context.Background(), &Bundle{Objects: objs}, Options{
		Client:       fc,
		Timeout:      50 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
	})
	if !errors.Is(err, ErrReadinessTimeout) {
		t.Fatalf("want readiness timeout, got %v", err)
	}
	// Created = namespace + deployment; the pre-existing ConfigMap is not owned.
	if len(res.Created) != 2 {
		t.Fatalf("want 2 created (ns + deployment), got %v", res.Created)
	}
	// Rollback happened, in REVERSE apply order: deployment first, ns last.
	if len(fc.deleteOrder) != 2 || !strings.HasPrefix(fc.deleteOrder[0], "Deployment|") || !strings.HasPrefix(fc.deleteOrder[1], "Namespace|") {
		t.Fatalf("rollback must delete in reverse apply order, got %v", fc.deleteOrder)
	}
	// The pre-existing ConfigMap survived.
	if _, ok := fc.store["ConfigMap|demo|cfg"]; !ok {
		t.Fatal("rollback deleted an object this run did NOT create")
	}
	if len(res.RolledBack) != 2 || len(res.CleanupFailed) != 0 {
		t.Fatalf("accounting wrong: %+v", res)
	}
	// The error reports what was cleaned.
	if !strings.Contains(err.Error(), "rolled back all 2") {
		t.Fatalf("error must report the rollback accounting, got %v", err)
	}
}

// A cleanup that cannot delete an object reports EXACTLY what was left
// behind — a partial apply is never silent.
func TestApplyBundleReportsCleanupFailures(t *testing.T) {
	fc := newFakeClient()
	fc.deleteErr["Namespace||demo"] = errors.New("webhook denied")
	dep := newObj("apps/v1", "Deployment", "demo", "web")
	setNested(dep, int64(1), "spec", "replicas")
	setNested(dep, int64(1), "metadata", "generation")
	objs := []*unstructured.Unstructured{newObj("v1", "Namespace", "", "demo"), dep}

	res, err := ApplyBundle(context.Background(), &Bundle{Objects: objs}, Options{
		Client:       fc,
		Timeout:      50 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("want failure")
	}
	if len(res.RolledBack) != 1 || len(res.CleanupFailed) != 1 {
		t.Fatalf("want 1 rolled back + 1 cleanup-failed, got %+v", res)
	}
	if !strings.Contains(res.CleanupFailed[0], "Namespace demo") || !strings.Contains(res.CleanupFailed[0], "webhook denied") {
		t.Fatalf("cleanup failure must name the object and reason: %v", res.CleanupFailed)
	}
	if !strings.Contains(err.Error(), "NOT cleaned") {
		t.Fatalf("error must call out what was left behind, got %v", err)
	}
}

// DisableRollback keeps everything in place but still REPORTS it.
func TestApplyBundleDisableRollbackLeavesAndReports(t *testing.T) {
	fc := newFakeClient()
	dep := newObj("apps/v1", "Deployment", "demo", "web")
	setNested(dep, int64(1), "spec", "replicas")
	setNested(dep, int64(1), "metadata", "generation")

	res, err := ApplyBundle(context.Background(), &Bundle{Objects: []*unstructured.Unstructured{dep}}, Options{
		Client:          fc,
		Timeout:         50 * time.Millisecond,
		PollInterval:    10 * time.Millisecond,
		DisableRollback: true,
	})
	if !errors.Is(err, ErrReadinessTimeout) {
		t.Fatalf("want readiness timeout, got %v", err)
	}
	if len(fc.deleteOrder) != 0 {
		t.Fatalf("rollback disabled must delete nothing, got %v", fc.deleteOrder)
	}
	if len(res.Created) != 1 || !strings.Contains(err.Error(), "LEFT IN PLACE") {
		t.Fatalf("disabled rollback must still report what was created: %+v / %v", res, err)
	}
}

// --- CRD gate (requirement 3) ----------------------------------------------

// A CR whose CRD ships in the same bundle is applied only after the CRD
// is Established AND its type shows up in discovery.
func TestApplyBundleGatesCRsBehindCRDEstablishedAndDiscovery(t *testing.T) {
	fc := newFakeClient()
	crd := newCRD()
	cr := newObj("example.io/v1", "Widget", "demo", "w1")
	objs := []*unstructured.Unstructured{cr, crd, newObj("v1", "Namespace", "", "demo")}
	sortByApplyRank(objs)

	// The CRD becomes Established on the 2nd Get; discovery serves the
	// type only after the 1st ServesGVK probe.
	fc.onGet["CustomResourceDefinition||widgets.example.io"] = func(call int, obj *unstructured.Unstructured) {
		if call >= 2 {
			setNested(obj, []any{map[string]any{"type": "Established", "status": "True"}}, "status", "conditions")
		}
	}
	fc.servedAfter["example.io/v1, Kind=Widget"] = 1

	res, err := ApplyBundle(context.Background(), &Bundle{Objects: objs}, Options{
		Client:         fc,
		Timeout:        2 * time.Second,
		PollInterval:   5 * time.Millisecond,
		CRDWaitTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	if res.Applied != 3 {
		t.Fatalf("want 3 applied, got %+v", res)
	}
	// The CR must have been applied strictly AFTER the CRD.
	crdIdx, crIdx := -1, -1
	for i, k := range fc.applyOrder {
		if strings.HasPrefix(k, "CustomResourceDefinition|") {
			crdIdx = i
		}
		if strings.HasPrefix(k, "Widget|") {
			crIdx = i
		}
	}
	if crdIdx == -1 || crIdx == -1 || crIdx < crdIdx {
		t.Fatalf("CR must apply after its CRD, order=%v", fc.applyOrder)
	}
	// Discovery was affirmatively probed before the CR went in.
	if fc.servesCalls["example.io/v1, Kind=Widget"] < 2 {
		t.Fatalf("discovery must be probed until the type appears, calls=%d", fc.servesCalls["example.io/v1, Kind=Widget"])
	}
}

// A CRD that never reaches discovery is a bounded, non-zero failure —
// never "apply the CR and hope".
func TestApplyBundleCRDGateTimesOut(t *testing.T) {
	fc := newFakeClient()
	crd := newCRD()
	cr := newObj("example.io/v1", "Widget", "demo", "w1")
	objs := []*unstructured.Unstructured{crd, cr}
	sortByApplyRank(objs)

	// Established immediately, but discovery NEVER serves the type.
	fc.onGet["CustomResourceDefinition||widgets.example.io"] = func(_ int, obj *unstructured.Unstructured) {
		setNested(obj, []any{map[string]any{"type": "Established", "status": "True"}}, "status", "conditions")
	}
	fc.notServed["example.io/v1, Kind=Widget"] = true

	_, err := ApplyBundle(context.Background(), &Bundle{Objects: objs}, Options{
		Client:         fc,
		Timeout:        time.Second,
		PollInterval:   5 * time.Millisecond,
		CRDWaitTimeout: 50 * time.Millisecond,
	})
	if !errors.Is(err, ErrCRDNotReady) {
		t.Fatalf("undiscoverable CRD type must fail with ErrCRDNotReady, got %v", err)
	}
	// The CR was never applied.
	for _, k := range fc.applyOrder {
		if strings.HasPrefix(k, "Widget|") {
			t.Fatalf("CR must not be applied when its CRD never reached discovery, order=%v", fc.applyOrder)
		}
	}
}

// Zero-desired flows through ApplyBundle end to end: terminal without the
// opt-in, converging with it.
func TestApplyBundleZeroDesiredEndToEnd(t *testing.T) {
	mk := func() (*fakeClient, *Bundle) {
		fc := newFakeClient()
		dep := newObj("apps/v1", "Deployment", "demo", "web")
		setNested(dep, int64(0), "spec", "replicas")
		setNested(dep, int64(1), "metadata", "generation")
		fc.onGet["Deployment|demo|web"] = func(_ int, obj *unstructured.Unstructured) {
			setNested(obj, int64(1), "status", "observedGeneration")
		}
		return fc, &Bundle{Objects: []*unstructured.Unstructured{dep}}
	}

	fc, b := mk()
	_, err := ApplyBundle(context.Background(), b, Options{
		Client: fc, Timeout: time.Second, PollInterval: 5 * time.Millisecond,
	})
	if !errors.Is(err, ErrZeroDesiredWorkload) {
		t.Fatalf("zero-desired without opt-in must fail with ErrZeroDesiredWorkload, got %v", err)
	}

	fc, b = mk()
	res, err := ApplyBundle(context.Background(), b, Options{
		Client: fc, Timeout: time.Second, PollInterval: 5 * time.Millisecond,
		AllowScaleToZero: true,
	})
	if err != nil {
		t.Fatalf("zero-desired WITH opt-in must converge: %v", err)
	}
	if res.Ready != 1 {
		t.Fatalf("want ready=1, got %+v", res)
	}
}
