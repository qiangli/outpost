// Package nodecap turns a runtime-probe DaemonSet into truthful
// scheduler state.
//
// WHY. A node reporting Ready is a KUBELET HEARTBEAT, not proof that the
// container runtime can create a pod sandbox. A node whose runtime is
// broken still goes Ready, the scheduler still places work there, and the
// symptom surfaces as a pod stuck in ContainerCreating rather than as a
// broken node — so the operator debugs the workload instead of the host.
// This is the same failure recorded elsewhere as "Running lies while
// crash-looping".
//
// The probe is a DaemonSet that TOLERATES THE TAINT IT OWNS, so a broken
// node keeps retrying sandbox creation and can demonstrate recovery.
// CoreDNS and ordinary workloads do not tolerate it, so they stay off.
//
// The label/taint/annotation vocabulary is deliberately IDENTICAL to the
// cloud-hosted control plane's. A workload's tolerations and node
// selectors must behave the same whichever placement runs its cluster
// (dhnt/docs/dks-control-plane-on-sphere.md) — divergent vocabulary would
// make manifests non-portable between placements, which is precisely what
// "placement is a configuration choice" promises it will not do.
package nodecap

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// ProbeLabelSelector selects the runtime-probe DaemonSet's pods.
	ProbeLabelSelector = "app.kubernetes.io/name=dks-runtime-probe"
	// ProbeNamespace is where the probe DaemonSet runs.
	ProbeNamespace = "kube-system"

	// UnavailableTaint keeps ordinary workloads off a node whose runtime
	// cannot create a sandbox. The probe itself tolerates it.
	UnavailableTaint = "outpost.dhnt.io/runtime-unavailable"
	// ReadyLabel is the positive form, for node selectors.
	ReadyLabel = "outpost.dhnt.io/runtime-ready"
	// UnavailableReason annotates WHY, so `kubectl describe node` explains
	// itself instead of showing a bare taint.
	UnavailableReason = "outpost.dhnt.io/runtime-unavailable-reason"

	// ProbeGrace is how long a not-yet-Ready probe is given before its
	// node is declared unavailable. Without it every node would be tainted
	// during the seconds between scheduling the probe and its first Ready,
	// which would evict workloads on every rollout.
	ProbeGrace = 2 * time.Minute

	unavailableReasonText = "runtime sandbox probe did not become Ready"
	taintValue            = "sandbox"
)

// PodReady reports whether a pod has PodReady=True.
func PodReady(p *corev1.Pod) bool {
	if p == nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// SetRuntimeCapability applies the probe's verdict to a node in memory.
// Returns (changed, available). It mutates node; callers persist it.
//
// Pure and separately tested because this is where the subtle behaviour
// lives — the grace period, and the fact that an ABSENT probe must never
// be read as failure.
func SetRuntimeCapability(node *corev1.Node, probe *corev1.Pod, now time.Time) (changed, available bool) {
	if node == nil || probe == nil {
		return false, false
	}
	available = PodReady(probe)

	// Not Ready yet, still inside the grace window: say nothing. A probe
	// that has simply not started is not evidence of a broken runtime.
	if !available && (probe.CreationTimestamp.IsZero() ||
		now.Sub(probe.CreationTimestamp.Time) < ProbeGrace) {
		return false, false
	}

	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}

	wantLabel := "false"
	if available {
		wantLabel = "true"
	}
	if node.Labels[ReadyLabel] != wantLabel {
		node.Labels[ReadyLabel] = wantLabel
		changed = true
	}

	if available {
		var removed bool
		node.Spec.Taints, removed = removeTaintByKey(node.Spec.Taints, UnavailableTaint)
		changed = changed || removed
		if _, ok := node.Annotations[UnavailableReason]; ok {
			delete(node.Annotations, UnavailableReason)
			changed = true
		}
		return changed, true
	}

	found := false
	for i := range node.Spec.Taints {
		if node.Spec.Taints[i].Key != UnavailableTaint {
			continue
		}
		found = true
		if node.Spec.Taints[i].Value != taintValue ||
			node.Spec.Taints[i].Effect != corev1.TaintEffectNoSchedule {
			node.Spec.Taints[i].Value = taintValue
			node.Spec.Taints[i].Effect = corev1.TaintEffectNoSchedule
			changed = true
		}
	}
	if !found {
		node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
			Key:       UnavailableTaint,
			Value:     taintValue,
			Effect:    corev1.TaintEffectNoSchedule,
			TimeAdded: &metav1.Time{Time: now},
		})
		changed = true
	}
	if node.Annotations[UnavailableReason] != unavailableReasonText {
		node.Annotations[UnavailableReason] = unavailableReasonText
		changed = true
	}
	return changed, false
}

func removeTaintByKey(taints []corev1.Taint, key string) ([]corev1.Taint, bool) {
	out := taints[:0:0]
	removed := false
	for _, t := range taints {
		if t.Key == key {
			removed = true
			continue
		}
		out = append(out, t)
	}
	if !removed {
		return taints, false
	}
	return out, true
}

// NodeFilter reports whether a node should be evaluated. Virtual-kubelet
// backends have no container runtime of their own to probe, so callers
// exclude them here rather than the package guessing from node names.
type NodeFilter func(*corev1.Node) bool

// Reconciler applies probe verdicts to nodes.
type Reconciler struct {
	Client   kubernetes.Interface
	Include  NodeFilter
	Interval time.Duration
	Log      *slog.Logger
	Now      func() time.Time // nil => time.Now
}

// Run reconciles until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	log := r.logger()
	if err := r.Once(ctx); err != nil {
		log.Debug("nodecap: initial reconcile failed (will retry)", "err", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Once(ctx); err != nil {
				log.Debug("nodecap: reconcile failed", "err", err)
			}
		}
	}
}

func (r *Reconciler) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// Once performs a single reconcile pass.
func (r *Reconciler) Once(ctx context.Context) error {
	if r.Client == nil {
		return fmt.Errorf("nodecap: Client is required")
	}
	log := r.logger()
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}

	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	pods, err := r.Client.CoreV1().Pods(ProbeNamespace).List(listCtx, metav1.ListOptions{
		LabelSelector: ProbeLabelSelector,
	})
	if err != nil {
		return fmt.Errorf("list runtime probes: %w", err)
	}
	byNode := probesByNode(pods.Items)

	nodes, err := r.Client.CoreV1().Nodes().List(listCtx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}

	for i := range nodes.Items {
		listed := &nodes.Items[i]
		if r.Include != nil && !r.Include(listed) {
			continue
		}
		probe := byNode[listed.Name]
		if probe == nil {
			// ABSENCE IS UNKNOWN, NOT FAILURE. The probe DaemonSet may
			// simply not have landed yet; tainting on missing evidence
			// would evict work from healthy nodes.
			continue
		}
		node := listed.DeepCopy()
		changed, available := SetRuntimeCapability(node, probe, now())
		if !changed {
			continue
		}
		upCtx, upCancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := r.Client.CoreV1().Nodes().Update(upCtx, node, metav1.UpdateOptions{})
		upCancel()
		if err != nil {
			log.Warn("nodecap: update failed", "node", node.Name, "err", err)
			continue
		}
		log.Info("nodecap: runtime capability", "node", node.Name, "ready", available)
	}
	return nil
}

// probesByNode picks one probe per node.
//
// A rolling DaemonSet update can briefly leave two probes on one node.
// ANY Ready probe is sufficient proof of capability; otherwise the OLDEST
// is used, so repeated replacement cannot indefinitely reset the failure
// grace period and hide a persistently broken runtime.
func probesByNode(items []corev1.Pod) map[string]*corev1.Pod {
	byNode := make(map[string]*corev1.Pod, len(items))
	for i := range items {
		p := &items[i]
		if p.Spec.NodeName == "" {
			continue
		}
		cur := byNode[p.Spec.NodeName]
		if cur == nil || PodReady(p) ||
			(!PodReady(cur) && p.CreationTimestamp.Before(&cur.CreationTimestamp)) {
			byNode[p.Spec.NodeName] = p
		}
	}
	return byNode
}
