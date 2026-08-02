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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

const (
	// ProbeLabelSelector selects the runtime-probe DaemonSet's pods.
	ProbeLabelSelector = "app.kubernetes.io/name=dks-runtime-probe"
	// ProbeNamespace is where the probe DaemonSet runs.
	ProbeNamespace = "kube-system"
	// ProbeDaemonSetName is the name of the probe DaemonSet.
	ProbeDaemonSetName = "dks-runtime-probe"
	// DefaultProbeImage is the default container image for the probe.
	// Reused image guaranteed by k3s containerd; no cloudbox dependency.
	DefaultProbeImage = "rancher/mirrored-pause:3.6"

	// UnavailableTaint keeps ordinary workloads off a node whose runtime
	// cannot create a sandbox. The probe itself tolerates it.
	UnavailableTaint = "outpost.dhnt.io/runtime-unavailable"
	// ReadyLabel is the positive form, for node selectors.
	ReadyLabel = "outpost.dhnt.io/runtime-ready"
	// UnavailableReason annotates WHY, so `kubectl describe node` explains
	// itself instead of showing a bare taint.
	UnavailableReason = "outpost.dhnt.io/runtime-unavailable-reason"

	// RuntimeLabel identifies the runtime type of a node.
	RuntimeLabel = "outpost.dhnt.io/runtime"
	// RuntimeVirtual is the label value for virtual-kubelet nodes.
	RuntimeVirtual = "virtual"

	// ProbeGrace is how long a not-yet-Ready probe is given before its
	// node is declared unavailable. Without it every node would be tainted
	// during the seconds between scheduling the probe and its first Ready,
	// which would evict workloads on every rollout.
	ProbeGrace = 2 * time.Minute

	unavailableReasonText = "runtime sandbox probe did not become Ready"
	taintValue            = "sandbox"
)

// ConstructProbeDaemonSet builds the desired DaemonSet spec for the runtime probe.
func ConstructProbeDaemonSet(image string) *appsv1.DaemonSet {
	if image == "" {
		image = DefaultProbeImage
	}
	zeroGrace := int64(0)
	maxUnavail := intstr.FromString("25%")
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ProbeDaemonSetName,
			Namespace: ProbeNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "dks-runtime-probe",
				"app.kubernetes.io/managed-by": "nodecap",
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": "dks-runtime-probe",
				},
			},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type: appsv1.RollingUpdateDaemonSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{
					MaxUnavailable: &maxUnavail,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name": "dks-runtime-probe",
					},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      RuntimeLabel,
												Operator: corev1.NodeSelectorOpNotIn,
												Values:   []string{RuntimeVirtual},
											},
										},
									},
								},
							},
						},
					},
					Tolerations: []corev1.Toleration{
						{
							Key:      UnavailableTaint,
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
						{
							Key:      "node-role.kubernetes.io/master",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
						{
							Key:      "node-role.kubernetes.io/control-plane",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
						{
							Key:      "CriticalAddonsOnly",
							Operator: corev1.TolerationOpExists,
						},
					},
					TerminationGracePeriodSeconds: &zeroGrace,
					Containers: []corev1.Container{
						{
							Name:            "probe",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1m"),
									corev1.ResourceMemory: resource.MustParse("4Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("10m"),
									corev1.ResourceMemory: resource.MustParse("16Mi"),
								},
							},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: ptrToBool(false),
								ReadOnlyRootFilesystem:   ptrToBool(true),
								RunAsNonRoot:             ptrToBool(true),
								RunAsUser:                ptrToInt64(65534),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
						},
					},
				},
			},
		},
	}
}

func ptrToBool(b bool) *bool    { return &b }
func ptrToInt64(i int64) *int64 { return &i }

// EnsureProbeDaemonSet deploys or updates the runtime probe DaemonSet in kube-system.
func EnsureProbeDaemonSet(ctx context.Context, client kubernetes.Interface, image string) error {
	if client == nil {
		return fmt.Errorf("nodecap: Client is required")
	}
	if image == "" {
		image = DefaultProbeImage
	}
	desired := ConstructProbeDaemonSet(image)
	dsClient := client.AppsV1().DaemonSets(ProbeNamespace)

	existing, err := dsClient.Get(ctx, ProbeDaemonSetName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			_, err := dsClient.Create(ctx, desired, metav1.CreateOptions{})
			if err != nil && !errors.IsAlreadyExists(err) {
				return fmt.Errorf("create runtime probe daemonset: %w", err)
			}
			return nil
		}
		return fmt.Errorf("get runtime probe daemonset: %w", err)
	}

	if probeDaemonSetNeedsUpdate(existing, desired) {
		existing.Spec = desired.Spec
		existing.Labels = desired.Labels
		_, err := dsClient.Update(ctx, existing, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("update runtime probe daemonset: %w", err)
		}
	}
	return nil
}

// DeleteProbeDaemonSet removes the runtime probe DaemonSet from kube-system.
func DeleteProbeDaemonSet(ctx context.Context, client kubernetes.Interface) error {
	if client == nil {
		return nil
	}
	dsClient := client.AppsV1().DaemonSets(ProbeNamespace)
	err := dsClient.Delete(ctx, ProbeDaemonSetName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete runtime probe daemonset: %w", err)
	}
	return nil
}

func probeDaemonSetNeedsUpdate(existing, desired *appsv1.DaemonSet) bool {
	if len(existing.Spec.Template.Spec.Containers) == 0 || len(desired.Spec.Template.Spec.Containers) == 0 {
		return true
	}
	return existing.Spec.Template.Spec.Containers[0].Image != desired.Spec.Template.Spec.Containers[0].Image
}

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
	Client                kubernetes.Interface
	Include               NodeFilter
	Interval              time.Duration
	Log                   *slog.Logger
	Now                   func() time.Time // nil => time.Now
	ProbeImage            string           // empty => DefaultProbeImage
	DisableProbeDaemonSet bool             // true => delete probe DaemonSet
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

	if r.DisableProbeDaemonSet {
		if err := DeleteProbeDaemonSet(ctx, r.Client); err != nil {
			log.Warn("nodecap: cleanup probe daemonset failed", "err", err)
		}
		return nil
	}

	if err := EnsureProbeDaemonSet(ctx, r.Client, r.ProbeImage); err != nil {
		log.Warn("nodecap: ensure probe daemonset failed", "err", err)
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
