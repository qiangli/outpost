package vknode

import (
	"fmt"
	"log/slog"
	"strconv"

	corev1 "k8s.io/api/core/v1"
)

// TransientApps is the minimal surface vknode needs to publish
// running pods into the outpost's local app router so cloudbox can
// reach them through the existing matrix tunnel + /h/<host>/app/<name>
// proxy.
//
// Implemented by *internal/agent.AppRegistry (adapter in cmd/outpost/
// main.go). Kept as an interface here so vknode doesn't import
// internal/agent (which would cycle through agent → vknode).
type TransientApps interface {
	// Register associates name with the loopback URL (e.g.
	// "http://127.0.0.1:31080"). Returns an error if the name is
	// already registered with a different target.
	Register(name, target string) error
	// Unregister removes the name. No-op if not present.
	Unregister(name string)
}

// TransientAppName returns the deterministic AppRegistry name for a
// given pod. Includes UID short-prefix so two pods with the same
// (namespace, name) at different points in time don't collide on
// AppRegistry slots — important since DeletePod runs after a brief
// async gap and a CreatePod for a freshly-recreated pod (same NS/name,
// new UID) could otherwise race the cleanup.
//
// Format: vk-<namespace>-<name>-<uid8>. Periods and slashes are
// already disallowed in k8s namespace/name; underscores and dashes
// pass through unchanged.
func TransientAppName(namespace, name, uid string) string {
	short := uid
	if len(short) > 8 {
		short = short[:8]
	}
	if short == "" {
		short = "noid"
	}
	return "vk-" + namespace + "-" + name + "-" + short
}

// publishPod walks pod's containers and registers each (port → URL)
// mapping with apps. Multiple ports on the same pod register multiple
// app names — vk-<…>-<port>. Containers without an explicit hostPort
// are skipped: with no podman-side port publish there's nothing to
// route to. Callers should log + ignore errors here — publishing is
// best-effort glue, not part of the pod's correctness contract.
func publishPod(apps TransientApps, pod *corev1.Pod) {
	if apps == nil || pod == nil {
		return
	}
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.HostPort == 0 {
				continue
			}
			name := transientAppNameForPort(pod, int(p.HostPort))
			target := fmt.Sprintf("http://127.0.0.1:%d", p.HostPort)
			if err := apps.Register(name, target); err != nil {
				slog.Warn("vknode: transient app register failed",
					"name", name, "target", target, "err", err)
				continue
			}
			slog.Info("vknode: registered transient app",
				"name", name, "target", target,
				"pod", podKey(pod.Namespace, pod.Name))
		}
	}
}

// unpublishPod is the symmetric cleanup. Called from DeletePod.
func unpublishPod(apps TransientApps, pod *corev1.Pod) {
	if apps == nil || pod == nil {
		return
	}
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.HostPort == 0 {
				continue
			}
			name := transientAppNameForPort(pod, int(p.HostPort))
			apps.Unregister(name)
			slog.Info("vknode: unregistered transient app",
				"name", name, "pod", podKey(pod.Namespace, pod.Name))
		}
	}
}

// transientAppNameForPort tacks the hostPort onto the per-pod name so
// a multi-port pod gets distinct app entries. Single-port pods get
// the bare TransientAppName(...) so the cloudbox-side handler can
// look up "vk-<ns>-<name>-<uid8>" without knowing about the port.
func transientAppNameForPort(pod *corev1.Pod, port int) string {
	base := TransientAppName(pod.Namespace, pod.Name, string(pod.UID))
	// First port → no port suffix (most pods have one port). Extra
	// ports get "-<port>" to disambiguate.
	if firstHostPort(pod) == port {
		return base
	}
	return base + "-" + strconv.Itoa(port)
}

func firstHostPort(pod *corev1.Pod) int {
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.HostPort != 0 {
				return int(p.HostPort)
			}
		}
	}
	return 0
}
