package bundleapply

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Logf is the logging sink the Applier writes progress to. Defaults to a
// no-op; the CLI wires it to stderr. Kept as a plain func so this package
// imports no logging framework.
type Logf func(format string, args ...any)

// Options controls one apply run.
type Options struct {
	// Client is the Kubernetes surface to apply against (required).
	Client ResourceClient
	// Timeout bounds the readiness wait for the WHOLE bundle. Must be > 0
	// — a zero/negative timeout is rejected rather than treated as
	// "wait forever" (an unbounded wait can mask a stuck rollout).
	Timeout time.Duration
	// PollInterval is how often readiness is re-checked. Defaults to 2s
	// when unset.
	PollInterval time.Duration
	// CRDWaitTimeout bounds, per CRD, the wait for Established + the
	// served types appearing in discovery before dependent custom
	// resources are applied. Defaults to 60s when unset. Timing out is a
	// hard failure (ErrCRDNotReady), never "apply the CR and hope".
	CRDWaitTimeout time.Duration
	// AllowScaleToZero is the EXPLICIT opt-in that lets a workload with
	// zero desired replicas count as rolled out. Without it a
	// spec.replicas: 0 workload is a terminal failure — zero desired
	// satisfying "all replicas ready" is an absence-of-evidence green.
	AllowScaleToZero bool
	// DisableRollback keeps everything this run created in place on
	// failure, instead of the default best-effort cleanup. The result
	// still reports exactly what was created and left behind.
	DisableRollback bool
	// RollbackTimeout bounds the best-effort cleanup pass. Defaults to
	// 60s. The cleanup runs on a context detached from the caller's (a
	// cancelled apply must still get its bounded cleanup chance).
	RollbackTimeout time.Duration
	// Log receives progress lines. Defaults to no-op.
	Log Logf
}

// Result summarizes an apply run — including, on failure, the precise
// transactional accounting: what this run created, what the rollback
// removed, and what it could NOT remove. A partial apply is never left
// behind silently.
type Result struct {
	Applied int
	Ready   int
	// Created lists the objects (kind ns/name) that did not exist before
	// this run applied them — the rollback set. Pre-existing objects this
	// run reconciled are never in here and are never deleted.
	Created []string
	// RolledBack lists the created objects the failure cleanup deleted.
	RolledBack []string
	// CleanupFailed lists created objects the cleanup could NOT delete
	// (with the reason) — these were left behind and the operator must
	// remove them by hand.
	CleanupFailed []string
}

// WaitOptions bounds one readiness wait.
type WaitOptions struct {
	Timeout          time.Duration
	PollInterval     time.Duration
	AllowScaleToZero bool
	Log              Logf
}

// ApplyBundle applies every object in the bundle in order, then waits for
// the workloads to become Ready. It is the single top-level entry point.
//
// Ordering: objects are applied in applyRank order, so Namespaces (and
// other cluster-scoped prerequisites) land before namespaced objects.
//
// CRD gate: when the bundle ships a CustomResourceDefinition together
// with custom resources of the type it defines, each such CR is applied
// only after the CRD reports Established AND the type appears in
// apiserver discovery — bounded by Options.CRDWaitTimeout, hard failure
// on timeout. Applying immediately would race the discovery cache and
// fail intermittently.
//
// Idempotence: each Apply is a server-side apply under a stable field
// manager, so calling ApplyBundle twice with the same bundle converges.
//
// Transactionality: the run records which objects it CREATED (as opposed
// to reconciled). On any failure — apply error, CRD gate timeout,
// readiness timeout, terminal workload state — those created objects are
// deleted again in reverse apply order, best-effort and bounded, and the
// result + error report precisely what was and was not cleaned. Objects
// that existed before this run are never deleted.
//
// Readiness: after all objects are applied, WaitForReady blocks up to
// Options.Timeout. A timeout returns ErrReadinessTimeout — the operator
// entry point turns that into a non-zero exit.
func ApplyBundle(ctx context.Context, b *Bundle, opts Options) (Result, error) {
	var res Result
	if b == nil || len(b.Objects) == 0 {
		return res, ErrEmptyBundle
	}
	if opts.Client == nil {
		return res, fmt.Errorf("bundleapply: nil ResourceClient")
	}
	if opts.Timeout <= 0 {
		return res, fmt.Errorf("bundleapply: readiness timeout must be > 0 (got %v)", opts.Timeout)
	}
	log := opts.Log
	if log == nil {
		log = func(string, ...any) {}
	}
	crdTimeout := opts.CRDWaitTimeout
	if crdTimeout <= 0 {
		crdTimeout = 60 * time.Second
	}

	// CRDs shipped in THIS bundle, by the group/kind they define. Used to
	// gate the bundle's own CRs behind Established + discovery.
	crdFor := bundleCRDsByGroupKind(b.Objects)

	// created tracks, in apply order, the objects THIS run brought into
	// existence — the only things a failure is allowed to clean up.
	var created []*unstructured.Unstructured
	fail := func(cause error) (Result, error) {
		return failAndCleanup(ctx, opts, log, &res, created, cause)
	}

	gateChecked := map[schema.GroupVersionKind]bool{}
	for _, obj := range b.Objects {
		gvk := obj.GroupVersionKind()
		if crd, ok := crdFor[gvk.GroupKind()]; ok && obj.GetKind() != "CustomResourceDefinition" && !gateChecked[gvk] {
			if err := waitForCRDServed(ctx, opts.Client, crd, gvk, crdTimeout, opts.PollInterval, log); err != nil {
				return fail(err)
			}
			gateChecked[gvk] = true
		}

		// Create-detection BEFORE the apply: a Get that says NotFound
		// means this run is about to create the object (rollback set);
		// any other Get failure is a hard error — we will not apply into
		// a cluster we cannot even read.
		_, gerr := opts.Client.Get(ctx, obj)
		preExisting := gerr == nil
		if gerr != nil && !errors.Is(gerr, ErrNotFound) {
			return fail(fmt.Errorf("bundleapply: pre-apply check for %s %s: %w", obj.GetKind(), nsName(obj), gerr))
		}

		if _, err := opts.Client.Apply(ctx, obj); err != nil {
			return fail(err)
		}
		res.Applied++
		if !preExisting {
			created = append(created, obj)
			res.Created = append(res.Created, identity(obj))
		}
		log("applied %s %s", obj.GetKind(), nsName(obj))
	}

	ready, err := WaitForReady(ctx, opts.Client, b.Objects, WaitOptions{
		Timeout:          opts.Timeout,
		PollInterval:     opts.PollInterval,
		AllowScaleToZero: opts.AllowScaleToZero,
		Log:              log,
	})
	res.Ready = ready
	if err != nil {
		return fail(err)
	}
	return res, nil
}

// failAndCleanup is the single failure exit: it rolls back (unless
// disabled) what THIS run created, in reverse apply order, bounded and
// best-effort, and returns an error that reports the cause plus the
// precise cleanup accounting.
func failAndCleanup(ctx context.Context, opts Options, log Logf, res *Result, created []*unstructured.Unstructured, cause error) (Result, error) {
	if len(created) == 0 {
		return *res, cause
	}
	if opts.DisableRollback {
		return *res, fmt.Errorf("%w; rollback disabled — %d object(s) created by this run were LEFT IN PLACE: %s",
			cause, len(created), strings.Join(res.Created, ", "))
	}

	rollbackTimeout := opts.RollbackTimeout
	if rollbackTimeout <= 0 {
		rollbackTimeout = 60 * time.Second
	}
	// Detached from the caller's context: a cancelled or timed-out apply
	// still gets its bounded cleanup chance.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()

	// Reverse apply order (dependents before the prerequisites they
	// depend on) via the same deletion mechanics DeleteBundle uses for an
	// operator-initiated uninstall, so the two teardown paths can't drift.
	res.RolledBack, res.CleanupFailed = deleteReverse(cctx, opts.Client, reverseOf(created), log, "rolled back", "cleanup FAILED")

	if len(res.CleanupFailed) > 0 {
		return *res, fmt.Errorf("%w; rolled back %d of %d created object(s); NOT cleaned (remove by hand): %s",
			cause, len(res.RolledBack), len(created), strings.Join(res.CleanupFailed, ", "))
	}
	return *res, fmt.Errorf("%w; rolled back all %d object(s) created by this run: %s",
		cause, len(created), strings.Join(res.RolledBack, ", "))
}

// bundleCRDsByGroupKind indexes the bundle's CustomResourceDefinitions by
// the GroupKind they define (spec.group + spec.names.kind).
func bundleCRDsByGroupKind(objs []*unstructured.Unstructured) map[schema.GroupKind]*unstructured.Unstructured {
	out := map[schema.GroupKind]*unstructured.Unstructured{}
	for _, obj := range objs {
		if obj.GetKind() != "CustomResourceDefinition" {
			continue
		}
		group, _, _ := unstructured.NestedString(obj.Object, "spec", "group")
		kind, _, _ := unstructured.NestedString(obj.Object, "spec", "names", "kind")
		if group == "" || kind == "" {
			continue
		}
		out[schema.GroupKind{Group: group, Kind: kind}] = obj
	}
	return out
}

// waitForCRDServed blocks until the CRD reports Established AND the
// apiserver's discovery serves gvk — or the bounded wait expires
// (ErrCRDNotReady, a hard failure). Both observations are positive
// evidence; neither alone is sufficient (Established can precede the
// discovery cache catching up, and a discovery hit for a non-Established
// CRD would accept writes it may still reject).
func waitForCRDServed(ctx context.Context, client ResourceClient, crd *unstructured.Unstructured, gvk schema.GroupVersionKind, timeout, poll time.Duration, log Logf) error {
	if log == nil {
		log = func(string, ...any) {}
	}
	if poll <= 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	reason := "not checked yet"
	for {
		live, err := client.Get(ctx, crd)
		if err != nil {
			return fmt.Errorf("bundleapply: CRD gate for %s: %w", gvk, err)
		}
		st := evalCRD(live)
		if st.ready {
			served, err := client.ServesGVK(ctx, gvk)
			if err != nil {
				return fmt.Errorf("bundleapply: CRD gate for %s: %w", gvk, err)
			}
			if served {
				log("crd gate %s: Established and served by discovery", gvk)
				return nil
			}
			reason = "Established but not in discovery yet"
		} else {
			reason = st.reason
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("%w: %s for %s after %v (last: %s)", ErrCRDNotReady, crd.GetName(), gvk, timeout, reason)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("bundleapply: CRD gate cancelled: %w", ctx.Err())
		case <-time.After(min(poll, remaining)):
		}
	}
}

// WaitForReady blocks until every object reports Ready or the timeout
// elapses. It returns the count of objects confirmed Ready and a non-nil
// error on timeout (ErrReadinessTimeout) or on a terminal failure (e.g. a
// Pod that entered phase Failed, or a zero-desired workload without the
// explicit opt-in).
//
// Evidence invariant: a Get that fails (apiserver unreachable, object
// vanished) is a hard error, never treated as "ready". Readiness must be
// affirmatively observed on a live object.
func WaitForReady(ctx context.Context, client ResourceClient, objs []*unstructured.Unstructured, opts WaitOptions) (int, error) {
	log := opts.Log
	if log == nil {
		log = func(string, ...any) {}
	}
	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	deadline := time.Now().Add(opts.Timeout)

	// pending is the set still waiting on readiness. Objects whose kind
	// has no rollout concept resolve on the first pass.
	pending := append([]*unstructured.Unstructured(nil), objs...)
	confirmed := 0

	for {
		var still []*unstructured.Unstructured
		var lastReason string
		for _, obj := range pending {
			live, err := client.Get(ctx, obj)
			if err != nil {
				// Absence/unreachability is a failure, not readiness.
				return confirmed, fmt.Errorf("bundleapply: readiness check for %s %s: %w", obj.GetKind(), nsName(obj), err)
			}
			st := evalReadiness(live, opts.AllowScaleToZero)
			if st.terminal != nil {
				return confirmed, fmt.Errorf("bundleapply: %s %s failed: %w", obj.GetKind(), nsName(obj), st.terminal)
			}
			if st.ready {
				confirmed++
				log("ready   %s %s (%s)", obj.GetKind(), nsName(obj), st.reason)
				continue
			}
			lastReason = fmt.Sprintf("%s %s: %s", obj.GetKind(), nsName(obj), st.reason)
			still = append(still, obj)
		}
		pending = still
		if len(pending) == 0 {
			return confirmed, nil
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return confirmed, fmt.Errorf("%w after %v: %d of %d not ready (last: %s)",
				ErrReadinessTimeout, opts.Timeout, len(pending), len(objs), lastReason)
		}

		select {
		case <-ctx.Done():
			return confirmed, fmt.Errorf("bundleapply: readiness wait cancelled: %w", ctx.Err())
		case <-time.After(min(pollInterval, remaining)):
		}
	}
}

func nsName(obj *unstructured.Unstructured) string {
	if ns := obj.GetNamespace(); ns != "" {
		return ns + "/" + obj.GetName()
	}
	return obj.GetName()
}

// identity renders "Kind ns/name" — the stable human identity used in the
// Created / RolledBack / CleanupFailed accounting.
func identity(obj *unstructured.Unstructured) string {
	return obj.GetKind() + " " + nsName(obj)
}
