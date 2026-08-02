package bundleapply

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ErrDeletionTimeout is returned by WaitForGone when at least one object
// is still present after the bounded wait. The caller must exit non-zero
// on this error — same contract as ErrReadinessTimeout on the apply side.
var ErrDeletionTimeout = errors.New("bundleapply: timed out waiting for objects to be removed")

// deleteReverse deletes objs (already in the caller's desired delete
// order — reverse apply order, so dependents go before the prerequisites
// they depend on) and reports what was removed and what could not be.
// Deleting an object that is already gone is success, matching
// ResourceClient.Delete's own idempotence contract.
//
// Shared by the apply-failure rollback path (failAndCleanup) and
// DeleteBundle (an operator-initiated uninstall), so the two ways an
// object can be torn down never drift in ordering or accounting shape.
func deleteReverse(ctx context.Context, client ResourceClient, objs []*unstructured.Unstructured, log Logf, okVerb, failVerb string) (ok []string, failed []string) {
	for _, obj := range objs {
		if err := client.Delete(ctx, obj); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", identity(obj), err))
			log("%s %s: %v", failVerb, identity(obj), err)
			continue
		}
		ok = append(ok, identity(obj))
		log("%s %s", okVerb, identity(obj))
	}
	return ok, failed
}

// reverseOf returns a new slice with objs in reverse order — dependents
// before the cluster-scoped prerequisites they depend on (a Namespace
// applied first is deleted last).
func reverseOf(objs []*unstructured.Unstructured) []*unstructured.Unstructured {
	out := make([]*unstructured.Unstructured, len(objs))
	for i, obj := range objs {
		out[len(objs)-1-i] = obj
	}
	return out
}

// DeleteOptions controls one uninstall run.
type DeleteOptions struct {
	// Client is the Kubernetes surface to delete against (required).
	Client ResourceClient
	// Timeout bounds an optional wait for the deleted objects to actually
	// vanish (finalizers, garbage collection). Zero/negative skips the
	// wait entirely — the objects are deleted and the call returns
	// without confirming they are gone.
	Timeout time.Duration
	// PollInterval is how often the gone-check is re-run. Defaults to 2s.
	PollInterval time.Duration
	// Log receives progress lines. Defaults to no-op.
	Log Logf
}

// DeleteResult reports one uninstall run.
type DeleteResult struct {
	// Deleted lists the objects (kind ns/name) this run removed — in
	// reverse apply order.
	Deleted []string
	// Failed lists objects that could NOT be deleted (with the reason).
	// A non-empty Failed always accompanies a non-nil error.
	Failed []string
	// Gone is the count confirmed absent after the wait (only meaningful
	// when Options.Timeout > 0).
	Gone int
}

// DeleteBundle removes every object in the bundle, in reverse apply
// order (dependents before the Namespace/CRDs/RBAC they depend on),
// best-effort across all objects — one delete failure does not stop the
// rest from being attempted. When Options.Timeout > 0 it then waits,
// bounded, for the deleted objects to actually disappear.
//
// This is the uninstall counterpart of ApplyBundle: it reuses the exact
// deletion mechanics (deleteReverse) that ApplyBundle's failure path uses
// to roll back a partial apply, so an operator-initiated uninstall and an
// apply-triggered rollback can never drift in ordering or accounting
// shape.
//
// Installer lifecycle (see lifecycle.go): the delete set is the bundle
// objects PLUS every installer's declared outputs — the actual installed
// product, not just the bootstrap manifest. Namespaced outputs would fall
// with their Namespace anyway (the explicit delete just confirms it), but
// cluster-scoped outputs (a ClusterRoleBinding an installer created)
// survive a Namespace deletion and are only removed because they are
// declared. Deleting an already-gone object is success, so re-running an
// uninstall — or uninstalling after a TTL-reaped installer — converges.
func DeleteBundle(ctx context.Context, b *Bundle, opts DeleteOptions) (DeleteResult, error) {
	var res DeleteResult
	if b == nil || len(b.Objects) == 0 {
		return res, ErrEmptyBundle
	}
	if opts.Client == nil {
		return res, fmt.Errorf("bundleapply: nil ResourceClient")
	}
	log := opts.Log
	if log == nil {
		log = func(string, ...any) {}
	}

	objs, err := expandWithOutputs(b.Objects)
	if err != nil {
		// Contract violation — fail closed, never guess at the product.
		return res, err
	}
	reversed := reverseOf(objs)
	deleted, failed := deleteReverse(ctx, opts.Client, reversed, log, "deleted", "delete FAILED")
	res.Deleted = deleted
	res.Failed = failed
	if len(failed) > 0 {
		return res, fmt.Errorf("bundleapply: uninstall left %d of %d object(s) behind (remove by hand): %s",
			len(failed), len(objs), strings.Join(failed, ", "))
	}

	if opts.Timeout > 0 {
		gone, err := WaitForGone(ctx, opts.Client, reversed, WaitOptions{
			Timeout:      opts.Timeout,
			PollInterval: opts.PollInterval,
			Log:          log,
		})
		res.Gone = gone
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// WaitForGone blocks until every object in objs is confirmed absent
// (Get reports ErrNotFound) or the timeout elapses.
//
// Evidence invariant: a Get that fails for any reason OTHER than
// ErrNotFound (apiserver unreachable, permission error) is a hard
// failure, never treated as "gone" — the absence of a successful read is
// not evidence the object was removed.
func WaitForGone(ctx context.Context, client ResourceClient, objs []*unstructured.Unstructured, opts WaitOptions) (int, error) {
	log := opts.Log
	if log == nil {
		log = func(string, ...any) {}
	}
	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	deadline := time.Now().Add(opts.Timeout)

	pending := append([]*unstructured.Unstructured(nil), objs...)
	confirmed := 0

	for {
		var still []*unstructured.Unstructured
		var lastReason string
		for _, obj := range pending {
			_, err := client.Get(ctx, obj)
			if err == nil {
				lastReason = fmt.Sprintf("%s %s: still present", obj.GetKind(), nsName(obj))
				still = append(still, obj)
				continue
			}
			if !errors.Is(err, ErrNotFound) {
				return confirmed, fmt.Errorf("bundleapply: deletion check for %s %s: %w", obj.GetKind(), nsName(obj), err)
			}
			confirmed++
			log("gone    %s %s", obj.GetKind(), nsName(obj))
		}
		pending = still
		if len(pending) == 0 {
			return confirmed, nil
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return confirmed, fmt.Errorf("%w after %v: %d of %d still present (last: %s)",
				ErrDeletionTimeout, opts.Timeout, len(pending), len(objs), lastReason)
		}

		select {
		case <-ctx.Done():
			return confirmed, fmt.Errorf("bundleapply: deletion wait cancelled: %w", ctx.Err())
		case <-time.After(min(pollInterval, remaining)):
		}
	}
}
