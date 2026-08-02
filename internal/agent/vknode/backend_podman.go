package vknode

import (
	"context"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// podmanBackend realizes Pods as libpod containers on the local podman
// daemon. It is the original (and default) vknode substrate, factored
// out of Provider behind the Backend seam so a native-process backend
// can plug into the same Provider. All container-state mutation goes
// through the hand-rolled HTTP-over-unix Client (see client.go); libpod
// serializes per-container, so concurrent Ensure/Delete for distinct
// pods don't race.
//
// clusterID scopes everything this backend touches to ONE cluster (see
// ClusterLabel). A podman socket is a shared substrate: the supervised
// daemon's provider and a standalone outpost-vk pointed at a different
// control plane may both drive it concurrently. Every list/adopt/delete
// therefore consults ownsUIDMatched / the List partition below instead
// of trusting ManagedLabel alone. Empty clusterID = legacy unscoped
// mode (no CA to fingerprint): the backend stamps no ClusterLabel and
// claims only unlabeled containers.
type podmanBackend struct {
	client    *Client
	clusterID string
}

// ownsUIDMatched reports whether a managed container ALREADY MATCHED by
// PodUIDLabel belongs to this backend's cluster. Rules:
//
//   - same ClusterLabel (including both empty) → ours;
//   - no ClusterLabel (legacy pre-scoping container) → ours, because the
//     caller matched the pod UID first and UIDs are apiserver-minted
//     UUIDs — another cluster cannot hold a pod with this UID, so the
//     UID match is unambiguous proof of ownership. This is the fail-
//     closed migration path for legacy containers: apiserver-driven
//     adopt/delete works, reconcile-driven listing does not (see List);
//   - any OTHER non-empty ClusterLabel → not ours, never touch it. Also
//     covers the unscoped-backend-vs-scoped-container direction: a
//     legacy daemon never claims a container a scoped provider stamped.
//
// Only valid after a UID match — List must NOT use this (a legacy
// container with a non-matching UID is ambiguous and stays untouched).
func (b *podmanBackend) ownsUIDMatched(labels map[string]string) bool {
	owner := labels[ClusterLabel]
	return owner == b.clusterID || owner == ""
}

// Ensure builds the spec, ensures the image + named volumes exist, and
// creates+starts the container — or adopts one a prior outpost
// incarnation left running for this pod.UID (restart + port-label
// hydration instead of a 409 cascade). The Provider has already run the
// namespace gate and host-port allocation; caching + transient-app
// publishing happen Provider-side after this returns.
func (b *podmanBackend) Ensure(ctx context.Context, pod *corev1.Pod) error {
	spec, err := BuildSpecForCluster(pod, b.clusterID)
	if err != nil {
		return err
	}
	// Source-canonical build: if the pod carries build-source
	// annotations, ensure the target image is available locally
	// (building now or confirming it's cached). skipPull is true when a
	// local image was produced/found — bypass the registry pull, since
	// a localhost-tagged image can't resolve against any registry.
	skipPull, err := b.EnsureImageBuilt(ctx, pod)
	if err != nil {
		return fmt.Errorf("vknode: build for pod %s: %w", podKey(pod.Namespace, pod.Name), err)
	}
	// Pre-materialize HostPath / EmptyDir named volumes — libpod's
	// /containers/create does not auto-create named volumes referenced
	// via mounts, so the first-create and the daemon-restart adopt path
	// both need the volumes to exist first.
	if err := EnsureVolumesForPod(ctx, b.client, pod, b.clusterID); err != nil {
		return fmt.Errorf("vknode: ensure volumes for pod %s: %w", podKey(pod.Namespace, pod.Name), err)
	}

	// Adopt an existing container for this pod UID if one survived a
	// prior incarnation: read back the host-port labels we stamped at
	// original-create time so the in-memory pod (and the Provider's
	// publishPod) sees the SAME hostPort the original allocation chose.
	existing, err := b.findContainerByPodUID(ctx, string(pod.UID))
	if err != nil {
		return fmt.Errorf("vknode: lookup existing container for pod %s: %w", podKey(pod.Namespace, pod.Name), err)
	}
	if existing != "" {
		if ins, ierr := b.client.InspectContainer(ctx, existing); ierr == nil && ins != nil {
			HydratePodPortsFromLabels(pod, ins.Config.Labels)
		} else if ierr != nil {
			slog.Warn("vknode: inspect existing container for port hydration",
				"container", existing, "err", ierr)
		}
		if err := b.client.StartContainer(ctx, existing); err != nil && !IsConflict(err) {
			return fmt.Errorf("vknode: start existing container %s: %w", existing, err)
		}
		slog.Info("vknode: adopted existing container",
			"pod", podKey(pod.Namespace, pod.Name), "container", existing)
		return nil
	}

	// Honor Kubernetes imagePullPolicy for ordinary (non-build) pods.  The
	// peer-DKS image delivery path deliberately side-loads large conformance
	// images into a worker's local Podman store; unconditionally pulling here
	// made both Never and IfNotPresent unusable and attempted to contact a
	// fictitious registry for localhost-tagged images.
	if !skipPull {
		policy := pod.Spec.Containers[0].ImagePullPolicy
		switch policy {
		case corev1.PullNever, corev1.PullIfNotPresent:
			exists, err := b.client.ImageExists(ctx, spec.Image)
			if err != nil {
				return fmt.Errorf("vknode: check local image %q: %w", spec.Image, err)
			}
			if exists {
				skipPull = true
			} else if policy == corev1.PullNever {
				return fmt.Errorf("vknode: image %q is not present locally and imagePullPolicy is Never", spec.Image)
			}
		}
	}

	// First time we've seen this pod (or libpod lost the container).
	// Skipped when EnsureImageBuilt produced/found a local image or the
	// Kubernetes pull policy selected an existing side-loaded image.
	if !skipPull {
		if err := b.client.PullImage(ctx, spec.Image); err != nil {
			return fmt.Errorf("vknode: pull image %q: %w", spec.Image, err)
		}
	}
	created, err := b.client.CreateContainer(ctx, spec)
	if err != nil {
		return fmt.Errorf("vknode: create container for pod %s: %w", podKey(pod.Namespace, pod.Name), err)
	}
	for _, w := range created.Warnings {
		slog.Warn("vknode: libpod create warning",
			"pod", podKey(pod.Namespace, pod.Name), "warning", w)
	}
	if err := b.client.StartContainer(ctx, created.ID); err != nil {
		return fmt.Errorf("vknode: start container %s: %w", created.ID, err)
	}
	slog.Info("vknode: created container",
		"pod", podKey(pod.Namespace, pod.Name), "container", created.ID, "image", spec.Image)
	return nil
}

// Delete force-stops + removes the pod's container and reaps its
// per-pod EmptyDir volumes. A missing container is not an error.
func (b *podmanBackend) Delete(ctx context.Context, pod *corev1.Pod) error {
	cid, err := b.findContainerByPodUID(ctx, string(pod.UID))
	if err != nil {
		return fmt.Errorf("vknode: lookup container for pod %s: %w", podKey(pod.Namespace, pod.Name), err)
	}
	if cid != "" {
		// Force=true → stop then remove in one call. Volumes=true →
		// drop anonymous tmpfs/emptyDir volumes; named volumes stay.
		if err := b.client.RemoveContainer(ctx, cid, true, true); err != nil && !IsNotFound(err) {
			return fmt.Errorf("vknode: remove container %s: %w", cid, err)
		}
	}
	// Reap per-pod EmptyDir-backed libpod volumes. Best-effort — a
	// leftover volume is inspectable and operator-droppable; not a
	// correctness issue.
	if err := RemoveEmptyDirsForPod(ctx, b.client, pod); err != nil {
		slog.Warn("vknode: remove emptyDir volumes",
			"pod", podKey(pod.Namespace, pod.Name), "err", err)
	}
	slog.Info("vknode: deleted pod",
		"pod", podKey(pod.Namespace, pod.Name), "container", cid)
	return nil
}

// Status inspects the pod's single container and translates it to a
// corev1.PodStatus. Returns (nil, nil) when the container has vanished
// so the Provider can surface Pending/ContainerMissing.
func (b *podmanBackend) Status(ctx context.Context, pod *corev1.Pod) (*corev1.PodStatus, error) {
	cid, err := b.findContainerByPodUID(ctx, string(pod.UID))
	if err != nil {
		return nil, err
	}
	if cid == "" {
		return nil, nil
	}
	inspect, err := b.client.InspectContainer(ctx, cid)
	if err != nil {
		return nil, err
	}
	status := inspectToPodStatus(ctx, pod, inspect)
	if pod.Annotations[TerminationLogTailAnnotation] == "true" &&
		len(status.ContainerStatuses) == 1 &&
		status.ContainerStatuses[0].State.Terminated != nil {
		logs, logErr := b.client.ContainerLogs(ctx, cid)
		if logErr != nil {
			slog.Warn("vknode: read terminal container logs",
				"pod", podKey(pod.Namespace, pod.Name), "container", cid, "err", logErr)
		} else {
			status.ContainerStatuses[0].State.Terminated.Message = terminationLogTail(logs)
			_ = logs.Close()
		}
	}
	return status, nil
}

// List rebuilds skeleton Pods from libpod's managed containers (those
// carrying ManagedLabel=true) that belong to THIS cluster. The
// reconstruction is intentionally minimal — the apiserver is the source
// of truth for the full spec; the PodController issues an UpdatePod
// with the real Pod once it lists.
//
// Ownership partition (the load-bearing safety boundary — whatever List
// returns, the PodController garbage-collects when its apiserver
// doesn't know the pod, so listing a container is a license to delete
// it):
//
//   - ClusterLabel == b.clusterID → ours, listed;
//   - ClusterLabel set but different → another cluster's, skipped
//     silently;
//   - ClusterLabel absent → legacy pre-scoping container: AMBIGUOUS.
//     Fail closed — not listed (so never GC'd), not adopted here. If it
//     is really ours, the apiserver still knows the pod and CreatePod's
//     UID-matched adopt path (ownsUIDMatched) recovers it; if it
//     belongs to another cluster, that cluster does the same. Scoped ↔
//     unscoped symmetry: an unscoped backend (clusterID == "") lists
//     only unlabeled containers and skips every claimed one.
//
// We deliberately fetch with the broad ManagedLabel filter and partition
// client-side rather than pushing ClusterLabel into the libpod filter:
// the client-side rule is the single enforcement point (exercised
// directly by the two-cluster tests) and it lets us log the legacy
// containers we're leaving alone instead of silently not seeing them.
func (b *podmanBackend) List(ctx context.Context) ([]*corev1.Pod, error) {
	items, err := b.client.ListContainers(ctx, true, map[string]string{ManagedLabel: "true"})
	if err != nil {
		return nil, fmt.Errorf("vknode: list managed containers: %w", err)
	}
	out := make([]*corev1.Pod, 0, len(items))
	for _, item := range items {
		owner := item.Labels[ClusterLabel]
		if owner != b.clusterID {
			if owner == "" {
				// Legacy unscoped container — see the fail-closed rule
				// in the function comment.
				slog.Warn("vknode: skipping legacy unscoped managed container (fail-closed; adopt happens on the pod-UID match path)",
					"container", item.ID,
					"pod", item.Labels[PodNamespaceLabel]+"/"+item.Labels[PodNameLabel])
			}
			continue
		}
		ns := item.Labels[PodNamespaceLabel]
		name := item.Labels[PodNameLabel]
		uid := item.Labels[PodUIDLabel]
		cname := item.Labels[ContainerNameLabel]
		if ns == "" || name == "" || uid == "" {
			slog.Warn("vknode: managed container missing identity labels",
				"container", item.ID, "labels", item.Labels)
			continue
		}
		out = append(out, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      name,
				UID:       types.UID(uid),
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  cname,
					Image: item.Image,
					Ports: portsFromLabels(item.Labels),
				}},
			},
		})
	}
	return out, nil
}

// HydratePorts merges the running container's stamped host-port labels
// back onto pod.Spec in place. Best-effort: a missing container is a
// no-op (the readinessProbe resolver falls back), never an error.
func (b *podmanBackend) HydratePorts(ctx context.Context, pod *corev1.Pod) error {
	if cid, lerr := b.findContainerByPodUID(ctx, string(pod.UID)); lerr == nil && cid != "" {
		if ins, ierr := b.client.InspectContainer(ctx, cid); ierr == nil && ins != nil {
			HydratePodPortsFromLabels(pod, ins.Config.Labels)
		}
	}
	return nil
}

// findContainerByPodUID returns the libpod container ID for the given
// pod UID, or "" if no managed container OWNED BY THIS CLUSTER matches.
// Errors only on list-level failures — an empty result is a normal "not
// here yet / already gone" signal.
//
// Ownership is ownsUIDMatched: same ClusterLabel, or a legacy container
// with no ClusterLabel (the UID match itself disambiguates — this is
// how pre-scoping containers get adopted and eventually replaced by
// labeled ones as pods churn). A container claimed by a DIFFERENT
// cluster is reported as not-found even on a UID match, so adopt,
// status, hydrate, and delete all fail closed against it. Because
// ContainerName is UID-derived, a synthetic cross-cluster UID collision
// then surfaces as a loud create-name conflict rather than a silent
// takeover — exactly what we want.
func (b *podmanBackend) findContainerByPodUID(ctx context.Context, podUID string) (string, error) {
	if podUID == "" {
		return "", nil
	}
	items, err := b.client.ListContainers(ctx, true, map[string]string{
		ManagedLabel: "true",
		PodUIDLabel:  podUID,
	})
	if err != nil {
		return "", err
	}
	owned := items[:0]
	for _, it := range items {
		if b.ownsUIDMatched(it.Labels) {
			owned = append(owned, it)
			continue
		}
		slog.Warn("vknode: pod UID matches a container owned by another cluster — leaving it alone",
			"pod_uid", podUID, "container", it.ID, "owner", it.Labels[ClusterLabel], "self", b.clusterID)
	}
	if len(owned) == 0 {
		return "", nil
	}
	if len(owned) > 1 {
		// Shouldn't happen — ContainerName is deterministic per podUID.
		// Prefer a running one; log so we can investigate.
		slog.Warn("vknode: multiple containers match pod UID",
			"pod_uid", podUID, "count", len(owned))
		for _, it := range owned {
			if it.State == "running" {
				return it.ID, nil
			}
		}
	}
	return owned[0].ID, nil
}
