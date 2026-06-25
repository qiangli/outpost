package vknode

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// hostPathVolumeName is the deterministic libpod volume name for a K8s
// HostPath volume. Keyed by (namespace, path) so two pods in the same
// namespace declaring the same HostPath share storage — which is the
// K8s HostPath contract ("this exact directory on the host"). The
// namespace gate prevents cross-tenant collisions on a path everyone
// might happen to pick (/data, /var/lib/x).
//
// Format: outpost-hp-<sha1(ns + "\x00" + path)[:16]>.
// The hash collapses arbitrary path characters into the libpod volume
// name alphabet ([a-zA-Z0-9_.-]) without us having to escape; 16 hex
// chars = 64 bits of namespace, ample for "no collisions across the
// volumes any one outpost holds".
//
// We use a libpod-managed named volume instead of a host bind mount
// because on macOS podman runs in a vfkit/libkrun Linux VM — bind-
// mounting "/tmp/x" or "/Users/qiangli/y" from the host into a
// container fails with "no such file or directory" since those paths
// don't exist inside the VM. Named volumes live inside the VM's own
// storage (/var/home/core/.local/share/containers/storage/volumes/),
// so they "just work" on both macOS and Linux outposts and still
// survive container removal — which is what the SeaweedFS-style
// "Deployment recreates the pod, data should persist" use case needs.
func hostPathVolumeName(namespace, path string) string {
	sum := sha1.Sum([]byte(namespace + "\x00" + path))
	return "outpost-hp-" + hex.EncodeToString(sum[:8])
}

// emptyDirVolumeName is the deterministic libpod volume name for a K8s
// EmptyDir volume. Keyed by (podUID, volumeName) so the lifetime
// tracks the Pod — DeletePod reaps both. A new Pod (different UID)
// declaring the same volume name gets a fresh volume, matching the
// K8s EmptyDir guarantee that data is per-Pod.
func emptyDirVolumeName(podUID, volumeName string) string {
	uid := strings.ReplaceAll(podUID, "-", "")
	if len(uid) > 12 {
		uid = uid[:12]
	}
	return "outpost-ed-" + uid + "-" + sanitizeVolName(volumeName)
}

func sanitizeVolName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// EnsureVolumesForPod pre-creates every named libpod volume the
// translator will reference for this Pod. Required because libpod's
// /containers/create endpoint does NOT auto-materialize named volumes
// — only `podman run -v name:/path` does, and that's a CLI-side
// convenience. Without this, the container starts then immediately
// fails with crun's "No such device" mount error.
//
// Idempotent: CreateVolume returns 409 for already-exists, which the
// client treats as success. Skips the Memory medium (tmpfs needs no
// volume) and any non-supported volume type (translator will reject
// those at BuildSpec time anyway).
//
// Labels stamped on each volume so an operator using
// `podman volume inspect <name>` / `podman volume ls --filter label=...`
// can recover which K8s namespace and HostPath / EmptyDir name is
// behind a given opaque outpost-* identifier.
func EnsureVolumesForPod(ctx context.Context, c *Client, pod *corev1.Pod) error {
	if pod == nil || c == nil {
		return nil
	}
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.HostPath != nil:
			name := hostPathVolumeName(pod.Namespace, v.HostPath.Path)
			labels := map[string]string{
				"outpost.io/managed":   "true",
				"outpost.io/kind":      "hostpath",
				"outpost.io/namespace": pod.Namespace,
				"outpost.io/hostpath":  v.HostPath.Path,
			}
			if err := c.CreateVolume(ctx, name, labels); err != nil {
				return fmt.Errorf("vknode: ensure hostPath volume %q: %w", name, err)
			}
		case v.EmptyDir != nil:
			if v.EmptyDir.Medium == corev1.StorageMediumMemory {
				continue
			}
			name := emptyDirVolumeName(string(pod.UID), v.Name)
			labels := map[string]string{
				"outpost.io/managed":     "true",
				"outpost.io/kind":        "emptydir",
				"outpost.io/namespace":   pod.Namespace,
				"outpost.io/pod-uid":     string(pod.UID),
				"outpost.io/volume-name": v.Name,
			}
			if err := c.CreateVolume(ctx, name, labels); err != nil {
				return fmt.Errorf("vknode: ensure emptyDir volume %q: %w", name, err)
			}
		}
	}
	return nil
}

// RemoveEmptyDirsForPod drops every libpod volume that DeletePod is
// responsible for cleaning up — namely the per-pod EmptyDir volumes,
// keyed by emptyDirVolumeName(podUID, volumeName). HostPath-derived
// volumes are NOT reaped here: their lifetime is "as long as the
// namespace wants the data", which DeletePod has no opinion on.
//
// Best-effort — individual volume removal failures are logged-not-
// returned so the larger DeletePod path still succeeds. A leftover
// volume becomes inspectable via `podman volume ls` (outpost-ed-*
// prefix) and the operator can drop it manually.
func RemoveEmptyDirsForPod(ctx context.Context, c *Client, pod *corev1.Pod) error {
	if pod == nil || c == nil {
		return nil
	}
	for _, v := range pod.Spec.Volumes {
		if v.EmptyDir == nil {
			continue
		}
		if v.EmptyDir.Medium == corev1.StorageMediumMemory {
			// tmpfs — never had a backing volume.
			continue
		}
		name := emptyDirVolumeName(string(pod.UID), v.Name)
		if err := c.RemoveVolume(ctx, name, true); err != nil {
			slog.Warn("vknode: remove emptyDir volume",
				"pod", pod.Namespace+"/"+pod.Name, "volume", v.Name, "name", name, "err", err)
		}
	}
	return nil
}
