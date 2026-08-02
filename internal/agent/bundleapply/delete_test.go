package bundleapply

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestDeleteBundle_RejectsNilInputs(t *testing.T) {
	if _, err := DeleteBundle(context.Background(), nil, DeleteOptions{Client: newFakeClient()}); !errors.Is(err, ErrEmptyBundle) {
		t.Fatalf("nil bundle: %v", err)
	}
	if _, err := DeleteBundle(context.Background(), &Bundle{}, DeleteOptions{Client: newFakeClient()}); !errors.Is(err, ErrEmptyBundle) {
		t.Fatalf("empty bundle: %v", err)
	}
	if _, err := DeleteBundle(context.Background(), statusBundleFixture(), DeleteOptions{}); err == nil {
		t.Fatal("nil client must be rejected")
	}
}

// Uninstall removes objects in REVERSE apply order — the namespaced
// object first, the Namespace it lives in last — the mirror image of
// apply order and identical to the rollback path's ordering.
func TestDeleteBundle_DeletesInReverseApplyOrder(t *testing.T) {
	fc := newFakeClient()
	b := statusBundleFixture() // [Namespace demo, ConfigMap demo/cfg]
	if _, err := ApplyBundle(context.Background(), b, Options{Client: fc, Timeout: time.Second, PollInterval: 5 * time.Millisecond}); err != nil {
		t.Fatalf("seed apply: %v", err)
	}

	res, err := DeleteBundle(context.Background(), b, DeleteOptions{Client: fc})
	if err != nil {
		t.Fatalf("DeleteBundle: %v", err)
	}
	if len(res.Deleted) != 2 {
		t.Fatalf("expected 2 deletions, got %+v", res)
	}
	if !strings.HasPrefix(res.Deleted[0], "ConfigMap ") || !strings.HasPrefix(res.Deleted[1], "Namespace ") {
		t.Fatalf("expected ConfigMap before Namespace (reverse apply order), got %v", res.Deleted)
	}
	if len(fc.store) != 0 {
		t.Fatalf("cluster should be empty after uninstall, got %v", fc.store)
	}

	// Deleting an object that no longer exists is success (idempotent),
	// matching ResourceClient.Delete's own contract.
	res2, err := DeleteBundle(context.Background(), b, DeleteOptions{Client: fc})
	if err != nil {
		t.Fatalf("second uninstall must be idempotent: %v", err)
	}
	if len(res2.Deleted) != 2 {
		t.Fatalf("re-uninstall should still report both as removed: %+v", res2)
	}
}

// A delete failure on one object does not stop the rest, and is reported
// precisely as an object the operator must remove by hand — same
// evidence contract as a failed rollback.
func TestDeleteBundle_ReportsPartialFailure(t *testing.T) {
	fc := newFakeClient()
	b := statusBundleFixture()
	if _, err := ApplyBundle(context.Background(), b, Options{Client: fc, Timeout: time.Second, PollInterval: 5 * time.Millisecond}); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	nsKey := keyOf(b.Objects[0])
	fc.deleteErr = map[string]error{nsKey: errors.New("namespace has active finalizers")}

	res, err := DeleteBundle(context.Background(), b, DeleteOptions{Client: fc})
	if err == nil {
		t.Fatal("partial delete failure must be a hard error")
	}
	if !strings.Contains(err.Error(), "remove by hand") {
		t.Fatalf("error must name the manual-removal contract, got %v", err)
	}
	if len(res.Failed) != 1 || len(res.Deleted) != 1 {
		t.Fatalf("expected 1 deleted + 1 failed, got %+v", res)
	}
}

// With a bounded wait, DeleteBundle confirms the objects actually vanish
// (not merely that Delete was accepted).
func TestDeleteBundle_WaitsForGone(t *testing.T) {
	fc := newFakeClient()
	b := statusBundleFixture()
	if _, err := ApplyBundle(context.Background(), b, Options{Client: fc, Timeout: time.Second, PollInterval: 5 * time.Millisecond}); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	res, err := DeleteBundle(context.Background(), b, DeleteOptions{Client: fc, Timeout: time.Second, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("DeleteBundle with wait: %v", err)
	}
	if res.Gone != 2 {
		t.Fatalf("expected both objects confirmed gone, got %+v", res)
	}
}

// WaitForGone: an object still present when the deadline elapses is a
// timeout, not a silent pass — and an apiserver read failure (anything
// but not-found) is a hard error, never treated as "gone".
func TestWaitForGone_TimeoutAndHardErrors(t *testing.T) {
	fc := newFakeClient()
	obj := newObj("v1", "ConfigMap", "demo", "cfg")
	fc.store[keyOf(obj)] = obj

	_, err := WaitForGone(context.Background(), fc, []*unstructured.Unstructured{obj}, WaitOptions{
		Timeout: 30 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	})
	if !errors.Is(err, ErrDeletionTimeout) {
		t.Fatalf("still-present object must time out, got %v", err)
	}

	fc.getErr = errors.New("dial tcp: connection refused")
	_, err = WaitForGone(context.Background(), fc, []*unstructured.Unstructured{obj}, WaitOptions{
		Timeout: 30 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	})
	if err == nil || errors.Is(err, ErrDeletionTimeout) {
		t.Fatalf("apiserver read failure must be a hard error, not a timeout or a pass: %v", err)
	}
}
