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
type podmanBackend struct {
	client *Client
}

// Ensure builds the spec, ensures the image + named volumes exist, and
// creates+starts the container — or adopts one a prior outpost
// incarnation left running for this pod.UID (restart + port-label
// hydration instead of a 409 cascade). The Provider has already run the
// namespace gate and host-port allocation; caching + transient-app
// publishing happen Provider-side after this returns.
func (b *podmanBackend) Ensure(ctx context.Context, pod *corev1.Pod) error {
	spec, err := BuildSpec(pod)
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
	if err := EnsureVolumesForPod(ctx, b.client, pod); err != nil {
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

	// First time we've seen this pod (or libpod lost the container).
	// Skipped when EnsureImageBuilt confirmed a local image.
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
	return inspectToPodStatus(ctx, pod, inspect), nil
}

// List rebuilds skeleton Pods from libpod's managed containers (those
// carrying ManagedLabel=true). The reconstruction is intentionally
// minimal — the apiserver is the source of truth for the full spec; the
// PodController issues an UpdatePod with the real Pod once it lists.
func (b *podmanBackend) List(ctx context.Context) ([]*corev1.Pod, error) {
	items, err := b.client.ListContainers(ctx, true, map[string]string{ManagedLabel: "true"})
	if err != nil {
		return nil, fmt.Errorf("vknode: list managed containers: %w", err)
	}
	out := make([]*corev1.Pod, 0, len(items))
	for _, item := range items {
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
// pod UID, or "" if no managed container matches. Errors only on
// list-level failures — an empty result is a normal "not here yet /
// already gone" signal.
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
	if len(items) == 0 {
		return "", nil
	}
	if len(items) > 1 {
		// Shouldn't happen — ContainerName is deterministic per podUID.
		// Prefer a running one; log so we can investigate.
		slog.Warn("vknode: multiple containers match pod UID",
			"pod_uid", podUID, "count", len(items))
		for _, it := range items {
			if it.State == "running" {
				return it.ID, nil
			}
		}
	}
	return items[0].ID, nil
}
