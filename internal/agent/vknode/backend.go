package vknode

import (
	"context"

	corev1 "k8s.io/api/core/v1"
)

// Backend is the substrate seam of the virtual-kubelet provider: it
// owns "make this Pod real on this host" while the Provider owns the
// virtual-kubelet contract (pod cache, namespace-access gate, host-port
// allocation, transient-app publishing). Splitting the two lets one
// vknode register a virtual Node whose Pods are realized by different
// mechanisms:
//
//   - podmanBackend (today) — Pods become libpod containers. On macOS/
//     Windows those run inside podman's Linux VM, so the host GPU is
//     not visible to them.
//   - a future native backend — Pods become native host processes
//     (e.g. a llama.cpp/ollama server), keeping full Metal/CUDA access.
//     That is the whole point of the seam: the Provider, NodeProvider,
//     cloudbox bootstrap, access gate, and kubeconfig plumbing are all
//     backend-agnostic and get reused unchanged.
//
// All methods are called from the Provider's PodLifecycleHandler
// methods, which already serialize per-pod via the apiserver's
// reconcile loop. A Backend may mutate the passed Pod in place (e.g.
// hydrating resolved host ports back onto Spec.Containers[*].Ports) —
// the Provider re-caches the Pod after each call.
type Backend interface {
	// Ensure creates+starts (or adopts an existing) workload for pod.
	// Idempotent: a workload already running for pod.UID is (re)started
	// rather than erroring on conflict. May mutate pod to hydrate the
	// resolved host ports the workload was actually published on.
	Ensure(ctx context.Context, pod *corev1.Pod) error

	// Delete stops+removes the workload for pod. Idempotent: a missing
	// workload (already cleaned up) is not an error.
	Delete(ctx context.Context, pod *corev1.Pod) error

	// Status returns the live PodStatus for pod's workload, or
	// (nil, nil) when the workload has vanished underneath us — the
	// Provider maps that to a Pending/ContainerMissing status so the
	// reconciler recreates it.
	Status(ctx context.Context, pod *corev1.Pod) (*corev1.PodStatus, error)

	// List returns skeleton Pods reconstructed from the workloads this
	// backend already owns on the host. Called once at startup so a
	// vknode restart doesn't lose track of what it created in a prior
	// lifetime.
	List(ctx context.Context) ([]*corev1.Pod, error)

	// HydratePorts best-effort merges the workload's resolved host
	// ports back onto pod.Spec in place (used by UpdatePod, where the
	// apiserver-side Pod never saw the outpost's local port
	// allocation). A missing workload is a no-op, not an error.
	HydratePorts(ctx context.Context, pod *corev1.Pod) error
}
