package vknode

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// HostPortLabelPrefix is the libpod-container label namespace
// vknode stamps to record auto-allocated host ports. One label per
// containerPort: `outpost.io/host-port-<containerPort>` → host port
// the container is actually published on. Used on daemon restart
// (Reconcile path) to re-derive the same in-memory pod.Spec.HostPort
// the original CreatePod allocated — so the transient AppRegistry
// entry survives without operators having to specify hostPort
// up-front in the Pod manifest.
const HostPortLabelPrefix = "outpost.io/host-port-"

// AllocateMissingHostPorts walks pod's containers and, for any
// containerPort with HostPort==0, grabs a free TCP port from the
// kernel (bind :0, read the assigned port, close), writes it back
// into the in-memory pod.Spec. The brief gap between close and
// podman's later bind is the standard "ephemeral allocation race"
// — acceptable here because the only thing competing for that port
// in practice is the operator's own apps, and a TCP-bind collision
// would surface immediately as a podman create error rather than
// silent corruption.
//
// Idempotent — pods that already have explicit hostPorts are left
// alone. Returns the count of newly-allocated ports for logging.
func AllocateMissingHostPorts(pod *corev1.Pod) (int, error) {
	if pod == nil {
		return 0, nil
	}
	count := 0
	for ci := range pod.Spec.Containers {
		c := &pod.Spec.Containers[ci]
		for pi := range c.Ports {
			p := &c.Ports[pi]
			if p.HostPort != 0 {
				continue
			}
			port, err := pickFreeTCPPort()
			if err != nil {
				return count, fmt.Errorf("vknode: allocate host port for %s/%s container=%s port=%d: %w",
					pod.Namespace, pod.Name, c.Name, p.ContainerPort, err)
			}
			p.HostPort = int32(port)
			slog.Info("vknode: auto-allocated host port",
				"pod", podKey(pod.Namespace, pod.Name),
				"container", c.Name,
				"containerPort", p.ContainerPort,
				"hostPort", p.HostPort)
			count++
		}
	}
	return count, nil
}

// pickFreeTCPPort returns a port the kernel is willing to lease right
// now. Binding :0 and reading back the assigned port is the canonical
// trick — the kernel won't hand the same port to another bind for a
// brief window after we close, which gives podman time to claim it.
func pickFreeTCPPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// LabelsForHostPorts emits the libpod labels that record pod's
// auto-allocated (or explicitly-set) host ports. Called at
// container-create time so HydratePodPortsFromLabels on a later
// daemon restart can reconstruct the same pod.Spec.HostPort values
// without having to re-allocate (which would pick different ports
// and break running clients).
func LabelsForHostPorts(pod *corev1.Pod) map[string]string {
	if pod == nil {
		return nil
	}
	out := map[string]string{}
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.HostPort == 0 {
				continue
			}
			out[HostPortLabelPrefix+strconv.Itoa(int(p.ContainerPort))] =
				strconv.Itoa(int(p.HostPort))
		}
	}
	return out
}

// portsFromLabels reconstructs a []ContainerPort from labels stamped
// by LabelsForHostPorts. Used by Reconcile when the skeleton pod is
// built from scratch (no apiserver-side spec available yet) so the
// transient app publish on the subsequent UpdatePod still has the
// right HostPort to register.
//
// Returns nil when no port labels are present (legacy containers
// from before this scheme landed, or pods that didn't have any
// containerPorts in the first place).
func portsFromLabels(labels map[string]string) []corev1.ContainerPort {
	var out []corev1.ContainerPort
	for k, v := range labels {
		if !strings.HasPrefix(k, HostPortLabelPrefix) {
			continue
		}
		cpStr := strings.TrimPrefix(k, HostPortLabelPrefix)
		cp, cerr := strconv.Atoi(cpStr)
		if cerr != nil || cp <= 0 || cp > 65535 {
			continue
		}
		hp, herr := strconv.Atoi(strings.TrimSpace(v))
		if herr != nil || hp <= 0 || hp > 65535 {
			continue
		}
		out = append(out, corev1.ContainerPort{
			ContainerPort: int32(cp),
			HostPort:      int32(hp),
			Protocol:      corev1.ProtocolTCP,
		})
	}
	return out
}

// HydratePodPortsFromLabels fills pod.Spec.Containers[].Ports[].HostPort
// from the labels stamped at container-create time. Used by the
// adopt + reconcile paths: at that point the in-memory pod skeleton
// has the containerPort (from the apiserver-side spec) but lost the
// auto-allocated HostPort that the original CreatePod chose. Reading
// the labels back is what makes the transient app registration
// survive daemon restart for pods that didn't have an explicit
// hostPort in their Pod manifest.
//
// Pre-existing HostPort values (explicit hostPort in the Pod spec)
// are left alone — the labels are only authoritative for ports that
// vknode allocated.
func HydratePodPortsFromLabels(pod *corev1.Pod, labels map[string]string) {
	if pod == nil {
		return
	}
	for ci := range pod.Spec.Containers {
		c := &pod.Spec.Containers[ci]
		for pi := range c.Ports {
			p := &c.Ports[pi]
			if p.HostPort != 0 {
				continue
			}
			v, ok := labels[HostPortLabelPrefix+strconv.Itoa(int(p.ContainerPort))]
			if !ok {
				continue
			}
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n <= 0 || n > 65535 {
				continue
			}
			p.HostPort = int32(n)
		}
	}
}
