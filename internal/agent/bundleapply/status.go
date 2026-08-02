package bundleapply

import (
	"context"
	"errors"
	"fmt"
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
}

// StatusResult is the bundle-wide status snapshot.
type StatusResult struct {
	Objects []ObjectStatus
	// Installed is true only when every object in the bundle exists on
	// the cluster.
	Installed bool
	// AllReady is true only when every object exists AND reports Ready.
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
		st := ObjectStatus{Kind: obj.GetKind(), Namespace: obj.GetNamespace(), Name: obj.GetName()}
		live, err := opts.Client.Get(ctx, obj)
		switch {
		case err == nil:
			st.Exists = true
			rs := evalReadiness(live, opts.AllowScaleToZero)
			if rs.terminal != nil {
				st.Reason = rs.terminal.Error()
			} else {
				st.Ready = rs.ready
				st.Reason = rs.reason
			}
		case errors.Is(err, ErrNotFound):
			st.Reason = "not found"
		default:
			return res, fmt.Errorf("bundleapply: status check for %s %s: %w", obj.GetKind(), nsName(obj), err)
		}
		if !st.Exists {
			res.Installed = false
		}
		if !st.Ready {
			res.AllReady = false
		}
		res.Objects = append(res.Objects, st)
	}
	return res, nil
}
