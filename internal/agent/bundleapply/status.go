package bundleapply

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// StatusOptions controls one status read.
type StatusOptions struct {
	// Client is the Kubernetes surface to read (required).
	Client ResourceClient
	// AllowScaleToZero mirrors Options.AllowScaleToZero: a workload with
	// spec.replicas: 0 reports Ready (scaled down on purpose) instead of
	// the terminal ErrZeroDesiredWorkload reason. Without it, a
	// zero-desired workload's Reason names the same terminal condition
	// ApplyBundle would have failed on — status never waits, it only
	// reports what a wait would have seen.
	AllowScaleToZero bool
}

// ObjectStatus is one bundle object's live state — a pure read, no apply
// or wait involved.
type ObjectStatus struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// Exists reports whether the object is present on the cluster at all.
	Exists bool `json:"exists"`
	// Ready reuses the exact rollout evaluation ApplyBundle's readiness
	// wait uses (evalReadiness) — a status Ready=true means the same
	// thing an apply's confirmed-ready count means.
	Ready bool `json:"ready"`
	// Reason is a short human string: why not-ready, why not-exists, or
	// the confirming detail when ready.
	Reason string `json:"reason,omitempty"`
	// Installer marks an object annotated outpost.dhnt.io/lifecycle=installer.
	// Its absence alone never decides installed-ness — the declared
	// outputs do (see the lifecycle contract in lifecycle.go).
	Installer bool `json:"installer,omitempty"`
	// DeclaredOutput marks a row synthesized from an installer's
	// outpost.dhnt.io/installs declaration rather than decoded from the
	// manifest — the durable post-install evidence being asserted.
	DeclaredOutput bool `json:"declared_output,omitempty"`
}

// StatusResult is the bundle-wide status snapshot.
type StatusResult struct {
	Objects []ObjectStatus
	// Installed is true only when every non-installer object exists on
	// the cluster AND every installer's declared durable outputs exist.
	// An installer's own absence never decides it (lifecycle contract).
	Installed bool
	// AllReady is true only when every asserted object — non-installer
	// bundle objects plus declared installer outputs — exists AND reports
	// Ready. A garbage-collected installer does not count against it; a
	// present-but-unready (e.g. failed) installer does.
	AllReady bool
}

// StatusBundle reports the live state of every object in the bundle
// without applying or deleting anything. It is a single pass — no
// polling, no bounded wait — built from the same evidence primitives
// ApplyBundle's readiness wait uses (Client.Get + evalReadiness), so
// "installed and ready" here means exactly what a successful ApplyBundle
// would have confirmed.
//
// Evidence invariant: a Get failure that is NOT "object not found" (an
// unreachable apiserver, a permission error) is a hard failure, never
// reported as "not installed" — the absence of a successful read is not
// evidence the object is absent.
//
// Installer lifecycle (see lifecycle.go): an object annotated
// outpost.dhnt.io/lifecycle=installer is judged by its DECLARED OUTPUTS, not
// by its own presence. A completed installer Job reaped by
// ttlSecondsAfterFinished does not make the bundle "not installed" as
// long as every declared durable output exists; conversely, a bundle is
// NEVER installed while any declared output is missing — a surviving
// Namespace/RBAC skeleton after a failed install stays installed=false.
func StatusBundle(ctx context.Context, b *Bundle, opts StatusOptions) (StatusResult, error) {
	var res StatusResult
	if b == nil || len(b.Objects) == 0 {
		return res, ErrEmptyBundle
	}
	if opts.Client == nil {
		return res, fmt.Errorf("bundleapply: nil ResourceClient")
	}

	res.Installed = true
	res.AllReady = true
	for _, obj := range b.Objects {
		st, err := liveStatus(ctx, opts, obj)
		if err != nil {
			return res, err
		}
		if !isInstaller(obj) {
			if !st.Exists {
				res.Installed = false
			}
			if !st.Ready {
				res.AllReady = false
			}
			res.Objects = append(res.Objects, st)
			continue
		}

		st.Installer = true
		declared, err := installerOutputs(obj)
		if err != nil {
			// Contract violation — fail closed, never guess.
			return res, err
		}
		outputsExist, outputsReady := true, true
		var outputs []ObjectStatus
		for _, d := range declared {
			ds, err := liveStatus(ctx, opts, d)
			if err != nil {
				return res, err
			}
			ds.DeclaredOutput = true
			if !ds.Exists {
				outputsExist = false
			}
			if !ds.Ready {
				outputsReady = false
			}
			outputs = append(outputs, ds)
		}
		switch {
		case st.Exists:
			// Installer still present (running, completed, or failed):
			// judge its own readiness normally; the declared outputs
			// below decide installed-ness either way.
			if !st.Ready {
				res.AllReady = false
			}
		case outputsExist:
			// Absent installer + every declared output present: the
			// installer completed and was garbage-collected (e.g.
			// ttlSecondsAfterFinished). Not evidence of absence.
			st.Reason = "not found; installer completed and was garbage-collected (declared outputs present)"
		default:
			// Absent installer + missing outputs: it never completed
			// (never ran, or failed and was reaped). Fail closed.
			st.Reason = "not found, and declared output(s) missing — installer never completed"
			res.Installed = false
			res.AllReady = false
		}
		if !outputsExist {
			res.Installed = false
		}
		if !outputsReady {
			res.AllReady = false
		}
		res.Objects = append(res.Objects, st)
		res.Objects = append(res.Objects, outputs...)
	}
	return res, nil
}

// liveStatus is one object's Get + readiness evaluation — the shared
// evidence read for bundle objects and declared installer outputs alike.
func liveStatus(ctx context.Context, opts StatusOptions, obj *unstructured.Unstructured) (ObjectStatus, error) {
	st := ObjectStatus{Kind: obj.GetKind(), Namespace: obj.GetNamespace(), Name: obj.GetName()}
	live, err := opts.Client.Get(ctx, obj)
	switch {
	case err == nil:
		st.Exists = true
		rs := evalReadiness(ctx, opts.Client, live, opts.AllowScaleToZero)
		if rs.terminal != nil {
			st.Reason = rs.terminal.Error()
		} else {
			st.Ready = rs.ready
			st.Reason = rs.reason
		}
	case errors.Is(err, ErrNotFound):
		st.Reason = "not found"
	default:
		return st, fmt.Errorf("bundleapply: status check for %s %s: %w", obj.GetKind(), nsName(obj), err)
	}
	return st, nil
}
