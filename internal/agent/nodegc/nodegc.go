// Package nodegc reclaims stale Kubernetes Node objects on a
// peer-hosted DKS control plane.
//
// WHY. A tunnelled worker that leaves the cluster (or simply dies)
// cannot delete its own Node object — `outpost cluster leave` holds
// only a k3s join token, deliberately not an admin credential for the
// peer apiserver (docs/cluster-peer.md, "Boundary: the Kubernetes Node
// object is not deleted by the worker"). And k3s agents register with
// `--with-node-id`, so every rejoin mints a NEW node name: without GC
// the plane accumulates NotReady ghosts that pollute `kubectl get
// nodes`, scheduling scores, and the status view forever.
//
// The collector is deliberately narrow and slow. It only ever deletes
// a node that is self-consistently OURS — exact runtime/backend labels,
// a nonempty host label, and a name carrying that host's prefix — and
// that has been NotReady/Unknown for longer than a 24 h grace period.
// Virtual-kubelet nodes (runtime=virtual), foreign or unlabelled nodes,
// renamed nodes, Ready nodes, recently-transitioned nodes, and nodes
// missing readiness evidence are never touched: absence of evidence is
// unknown, not staleness. Deletes are bounded per tick, re-verified by
// a fresh GET immediately beforehand, and guarded by a UID precondition
// so a same-name replacement registered between passes survives. Any
// API error stops the whole pass — a flaky apiserver is a reason to do
// nothing, not to guess.
//
// This runs ONLY where the plane it manages runs: the control-plane
// host. It needs no cloudbox peer list or liveness feed — the node's
// own recorded Ready transition is the staleness clock.
package nodegc

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	// RuntimeLabel / BackendLabel / HostLabel are the node-identity
	// labels the k3s agent entrypoint stamps at registration
	// (internal/agent/runtime/image/entrypoint.sh), matching the
	// cloud-hosted control plane's vocabulary exactly.
	RuntimeLabel = "outpost.dhnt.io/runtime"
	BackendLabel = "outpost.dhnt.io/backend"
	HostLabel    = "outpost.dhnt.io/host"

	// RuntimeAgent / BackendK3s are the only label values GC acts on.
	// Virtual-kubelet nodes carry runtime=virtual and a vk-* backend;
	// they have no kubelet lease of their own worth reaping here.
	RuntimeAgent = "agent"
	BackendK3s   = "k3s"

	// DefaultGrace is how long a node must have been NotReady/Unknown
	// (per its Ready condition's LastTransitionTime) before it is
	// stale. A day comfortably clears laptop sleeps, reboots, and
	// multi-hour network outages — the node of a machine that comes
	// back inside the window reconnects and goes Ready again.
	DefaultGrace = 24 * time.Hour

	// DefaultMaxDeletes bounds deletes per pass. GC converges over
	// several ticks instead of mass-deleting in one, so one bad pass
	// can only do bounded damage.
	DefaultMaxDeletes = 3

	// DefaultInterval is the pass cadence. Staleness is measured in
	// days; there is nothing to win by checking more often.
	DefaultInterval = time.Hour
)

// Collector deletes stale agent Node objects from the hosted plane.
type Collector struct {
	Client     kubernetes.Interface
	Grace      time.Duration    // 0 => DefaultGrace
	MaxDeletes int              // 0 => DefaultMaxDeletes
	Interval   time.Duration    // 0 => DefaultInterval
	Log        *slog.Logger     // nil => slog.Default
	Now        func() time.Time // nil => time.Now
}

// Run collects until ctx is cancelled. Pass errors are logged, never
// fatal — the next tick retries from scratch.
func (c *Collector) Run(ctx context.Context) {
	interval := c.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	log := c.logger()
	if err := c.Once(ctx); err != nil {
		log.Warn("nodegc: pass failed (will retry)", "err", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.Once(ctx); err != nil {
				log.Warn("nodegc: pass failed (will retry)", "err", err)
			}
		}
	}
}

func (c *Collector) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Collector) grace() time.Duration {
	if c.Grace > 0 {
		return c.Grace
	}
	return DefaultGrace
}

// Once performs a single bounded GC pass. Any List/Get/Delete error
// aborts the pass and is returned — never swallowed.
func (c *Collector) Once(ctx context.Context) error {
	if c.Client == nil {
		return fmt.Errorf("nodegc: Client is required")
	}
	log := c.logger()
	grace := c.grace()
	maxDeletes := c.MaxDeletes
	if maxDeletes <= 0 {
		maxDeletes = DefaultMaxDeletes
	}

	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	nodes, err := c.Client.CoreV1().Nodes().List(listCtx, metav1.ListOptions{})
	cancel()
	if err != nil {
		return fmt.Errorf("nodegc: list nodes: %w", err)
	}

	// Filtering happens HERE, in code, not in a server-side selector:
	// every exclusion rule is then a tested code path, and the pre-delete
	// re-evaluation below runs the identical predicate on the fresh GET.
	type candidate struct {
		name       string
		uid        types.UID
		transition time.Time
	}
	var stale []candidate
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if t, ok := StaleSince(n, grace, c.now()); ok {
			stale = append(stale, candidate{name: n.Name, uid: n.UID, transition: t})
		}
	}

	// Deterministic oldest-first: the longest-dead node goes first, and
	// equal timestamps fall back to name order so two passes over the
	// same state always pick the same bounded subset.
	sort.Slice(stale, func(i, j int) bool {
		if !stale[i].transition.Equal(stale[j].transition) {
			return stale[i].transition.Before(stale[j].transition)
		}
		return stale[i].name < stale[j].name
	})

	deleted := 0
	for _, cand := range stale {
		if deleted >= maxDeletes {
			log.Info("nodegc: per-pass delete budget reached", "deleted", deleted, "remaining", len(stale)-deleted)
			break
		}

		// Re-GET and fully re-evaluate immediately before deleting: the
		// list snapshot may be minutes stale, and a node that recovered
		// (or was replaced) in the meantime must survive.
		getCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		fresh, err := c.Client.CoreV1().Nodes().Get(getCtx, cand.name, metav1.GetOptions{})
		cancel()
		if err != nil {
			return fmt.Errorf("nodegc: re-get node %s: %w", cand.name, err)
		}
		if fresh.UID != cand.uid {
			// Same name, different object: a replacement registered
			// since the list. Not ours to judge this pass.
			log.Info("nodegc: node replaced since list, skipping", "node", cand.name)
			continue
		}
		if _, ok := StaleSince(fresh, grace, c.now()); !ok {
			log.Info("nodegc: node no longer stale, skipping", "node", cand.name)
			continue
		}

		uid := fresh.UID
		delCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = c.Client.CoreV1().Nodes().Delete(delCtx, cand.name, metav1.DeleteOptions{
			// UID precondition: the apiserver refuses the delete if the
			// object was replaced between our GET and this call.
			Preconditions: &metav1.Preconditions{UID: &uid},
		})
		cancel()
		if err != nil {
			return fmt.Errorf("nodegc: delete node %s: %w", cand.name, err)
		}
		deleted++
		log.Info("nodegc: deleted stale node", "node", cand.name,
			"not_ready_since", cand.transition.UTC().Format(time.RFC3339))
	}
	return nil
}

// StaleSince reports whether node is a stale GC candidate at time now,
// and if so since when (its Ready condition's LastTransitionTime).
//
// A candidate must be self-consistently an outpost k3s agent node:
// exact runtime/backend labels, a nonempty host label, and a name
// prefixed by that host — anything else (virtual, foreign, unlabelled,
// renamed) is out of scope. It must carry a Ready condition that is
// False or Unknown with a nonzero LastTransitionTime strictly older
// than grace; a missing condition or timestamp is unknown, not stale.
func StaleSince(node *corev1.Node, grace time.Duration, now time.Time) (time.Time, bool) {
	if node == nil {
		return time.Time{}, false
	}
	if node.Labels[RuntimeLabel] != RuntimeAgent || node.Labels[BackendLabel] != BackendK3s {
		return time.Time{}, false
	}
	host := node.Labels[HostLabel]
	if host == "" {
		return time.Time{}, false
	}
	// k3s `--with-node-id` names the node <host>-<id>; a name that does
	// not match its own host label is not an identity we recognize.
	if !strings.HasPrefix(node.Name, host+"-") {
		return time.Time{}, false
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type != corev1.NodeReady {
			continue
		}
		if cond.Status != corev1.ConditionFalse && cond.Status != corev1.ConditionUnknown {
			return time.Time{}, false
		}
		if cond.LastTransitionTime.IsZero() {
			return time.Time{}, false
		}
		if now.Sub(cond.LastTransitionTime.Time) <= grace {
			return time.Time{}, false
		}
		return cond.LastTransitionTime.Time, true
	}
	// No Ready condition at all: no evidence either way. Skip.
	return time.Time{}, false
}
