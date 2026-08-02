package bundleapply

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// statusBundleFixture uses two kinds with no rollout concept (Namespace,
// ConfigMap) — ready as soon as they exist, per evalReadiness — so
// ApplyBundle converges in one pass without a fake client needing to
// simulate a rollout across polls.
func statusBundleFixture() *Bundle {
	ns := newObj("v1", "Namespace", "", "demo")
	cm := newObj("v1", "ConfigMap", "demo", "cfg")
	return &Bundle{Objects: []*unstructured.Unstructured{ns, cm}}
}

func TestStatusBundle_RejectsNilInputs(t *testing.T) {
	if _, err := StatusBundle(context.Background(), nil, StatusOptions{Client: newFakeClient()}); !errors.Is(err, ErrEmptyBundle) {
		t.Fatalf("nil bundle: %v", err)
	}
	if _, err := StatusBundle(context.Background(), &Bundle{}, StatusOptions{Client: newFakeClient()}); !errors.Is(err, ErrEmptyBundle) {
		t.Fatalf("empty bundle: %v", err)
	}
	if _, err := StatusBundle(context.Background(), statusBundleFixture(), StatusOptions{}); err == nil {
		t.Fatal("nil client must be rejected")
	}
}

// Nothing applied yet: every object reports Exists=false, Installed=false,
// AllReady=false — status never applies anything, it only reads.
func TestStatusBundle_ReportsNotInstalled(t *testing.T) {
	fc := newFakeClient()
	res, err := StatusBundle(context.Background(), statusBundleFixture(), StatusOptions{Client: fc})
	if err != nil {
		t.Fatalf("StatusBundle: %v", err)
	}
	if res.Installed || res.AllReady {
		t.Fatalf("nothing applied yet must not report installed/ready: %+v", res)
	}
	if len(res.Objects) != 2 {
		t.Fatalf("expected 2 object statuses, got %d", len(res.Objects))
	}
	for _, o := range res.Objects {
		if o.Exists {
			t.Fatalf("object %+v should not exist", o)
		}
		if o.Reason != "not found" {
			t.Fatalf("expected 'not found' reason, got %q", o.Reason)
		}
	}
}

// Once applied and rolled out (via the exact same fake client + apply
// path ApplyBundle uses), StatusBundle reports the same verdict —
// StatusBundle reuses evalReadiness so "ready" means one thing everywhere.
func TestStatusBundle_ReflectsReadinessAfterApply(t *testing.T) {
	fc := newFakeClient()
	b := statusBundleFixture()
	res, err := ApplyBundle(context.Background(), b, Options{Client: fc, Timeout: time.Second, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	_ = res

	status, err := StatusBundle(context.Background(), b, StatusOptions{Client: fc})
	if err != nil {
		t.Fatalf("StatusBundle: %v", err)
	}
	if !status.Installed || !status.AllReady {
		t.Fatalf("applied+ready bundle must report installed and ready: %+v", status)
	}
	for _, o := range status.Objects {
		if !o.Exists || !o.Ready {
			t.Fatalf("object should be ready: %+v", o)
		}
	}
}

// A workload declaring spec.replicas: 0 is a TERMINAL condition in
// evalReadiness without the opt-in — StatusBundle must report that as
// not-ready-with-a-reason, not blow up or hang (status never waits).
func TestStatusBundle_ZeroDesiredIsNotReadyNotFatal(t *testing.T) {
	fc := newFakeClient()
	dep := newObj("apps/v1", "Deployment", "demo", "web")
	setNested(dep, int64(0), "spec", "replicas")
	fc.store[keyOf(dep)] = dep

	b := &Bundle{Objects: []*unstructured.Unstructured{dep}}
	status, err := StatusBundle(context.Background(), b, StatusOptions{Client: fc})
	if err != nil {
		t.Fatalf("StatusBundle: %v", err)
	}
	if status.AllReady {
		t.Fatalf("zero-desired without opt-in must not report ready: %+v", status)
	}
	if len(status.Objects) != 1 || !status.Objects[0].Exists || status.Objects[0].Ready {
		t.Fatalf("unexpected: %+v", status.Objects)
	}
}

// An unreachable apiserver (Get fails for a reason other than
// ErrNotFound) is a hard failure, never reported as "not installed" —
// the evidence invariant.
func TestStatusBundle_UnreachableAPIServerFailsClosed(t *testing.T) {
	fc := newFakeClient()
	fc.getErr = errors.New("dial tcp: connection refused")
	_, err := StatusBundle(context.Background(), statusBundleFixture(), StatusOptions{Client: fc})
	if err == nil {
		t.Fatal("unreachable apiserver must be a hard failure, not a status result")
	}
}
