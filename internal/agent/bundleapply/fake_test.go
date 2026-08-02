package bundleapply

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeClient is a deterministic in-memory ResourceClient — no apiserver,
// no kubeconfig, no network. It records apply/delete ordering (so tests
// can assert Namespace-before-namespaced and reverse-order rollback) and
// lets a test script readiness by mutating the stored object's status, or
// by supplying a per-object readiness hook that runs each time Get is
// called.
type fakeClient struct {
	mu sync.Mutex

	// store holds the "live" objects keyed by identity.
	store map[string]*unstructured.Unstructured
	// applyOrder records the identity keys in the order Apply saw them.
	applyOrder []string
	// applyCount counts applies per key (idempotence assertions).
	applyCount map[string]int
	// deleteOrder records the identity keys in the order Delete saw them
	// (rollback assertions).
	deleteOrder []string

	// getErr, when set, makes every Get fail (simulates an unreachable
	// apiserver / evidence invariant).
	getErr error
	// applyErr, when set for a key, makes Apply of that object fail.
	applyErr map[string]error
	// deleteErr, when set for a key, makes Delete of that object fail
	// (cleanup-failure reporting assertions).
	deleteErr map[string]error

	// notServed holds GVK strings ServesGVK reports as absent from
	// discovery; servedAfter lets a GVK appear after N ServesGVK calls.
	// Anything not mentioned is served (the common case: built-in types).
	notServed   map[string]bool
	servedAfter map[string]int
	servesCalls map[string]int

	// onGet, when set for a key, is invoked with the stored object before
	// it is returned — a test uses it to drive a rollout to Ready over
	// successive polls.
	onGet map[string]func(call int, obj *unstructured.Unstructured)
	// getCalls counts Get calls per key (drives onGet).
	getCalls map[string]int
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		store:       map[string]*unstructured.Unstructured{},
		applyCount:  map[string]int{},
		applyErr:    map[string]error{},
		deleteErr:   map[string]error{},
		notServed:   map[string]bool{},
		servedAfter: map[string]int{},
		servesCalls: map[string]int{},
		onGet:       map[string]func(int, *unstructured.Unstructured){},
		getCalls:    map[string]int{},
	}
}

func keyOf(obj *unstructured.Unstructured) string {
	return fmt.Sprintf("%s|%s|%s", obj.GetKind(), obj.GetNamespace(), obj.GetName())
}

func (f *fakeClient) Apply(_ context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := keyOf(obj)
	if err := f.applyErr[k]; err != nil {
		return nil, err
	}
	f.applyOrder = append(f.applyOrder, k)
	f.applyCount[k]++
	// Server-side apply is create-or-replace of the spec we sent; deep
	// copy so later status mutations don't alias the caller's object.
	f.store[k] = obj.DeepCopy()
	return f.store[k], nil
}

func (f *fakeClient) Get(_ context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	k := keyOf(obj)
	live, ok := f.store[k]
	if !ok {
		return nil, fmt.Errorf("fake: object %s: %w", k, ErrNotFound)
	}
	f.getCalls[k]++
	if hook := f.onGet[k]; hook != nil {
		hook(f.getCalls[k], live)
	}
	return live.DeepCopy(), nil
}

func (f *fakeClient) Delete(_ context.Context, obj *unstructured.Unstructured) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := keyOf(obj)
	if err := f.deleteErr[k]; err != nil {
		return err
	}
	f.deleteOrder = append(f.deleteOrder, k)
	delete(f.store, k)
	return nil
}

func (f *fakeClient) ServesGVK(_ context.Context, gvk schema.GroupVersionKind) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := gvk.String()
	f.servesCalls[k]++
	if after, ok := f.servedAfter[k]; ok {
		return f.servesCalls[k] > after, nil
	}
	if f.notServed[k] {
		return false, nil
	}
	return true, nil
}

// --- small unstructured builders used across tests -------------------------

func newObj(apiVersion, kind, ns, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{}}
	o.SetAPIVersion(apiVersion)
	o.SetKind(kind)
	if ns != "" {
		o.SetNamespace(ns)
	}
	o.SetName(name)
	return o
}

// setNested is a tiny helper to stamp a value at a path in an object.
func setNested(o *unstructured.Unstructured, val any, fields ...string) {
	if err := unstructured.SetNestedField(o.Object, val, fields...); err != nil {
		panic(err)
	}
}
