package vkpodman

import (
	"fmt"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// v1 supported-feature surface for the translator. Anything not listed
// here is rejected with a wrapped error at BuildSpec time so the user
// sees a clear "this Pod won't run on outpost yet" rather than a half-
// working container with silently-dropped fields. Growing this list is
// the v2 path.
//
//   * exactly one container per Pod (no init containers, no sidecars)
//   * env: only Value (no ValueFrom / EnvFrom)
//   * volumes: HostPath and EmptyDir only
//   * resources: limits only (requests are scheduling-only, libpod ignores)
//   * networking: HostNetwork=true OR a list of containerPorts
//   * security: ignored (we run as the agent's OS user via rootless podman)

// BuildSpec converts a corev1.Pod into a libpod SpecGenerator suitable
// for the /libpod/containers/create endpoint. Returns an error when the
// Pod uses a feature outside the v1 supported surface (see the file
// comment). The returned spec carries the outpost.io/* identity labels
// so reconcile and `podman ps` both stay informative.
func BuildSpec(pod *corev1.Pod) (*SpecGenerator, error) {
	if pod == nil {
		return nil, fmt.Errorf("vkpodman: nil Pod")
	}
	if len(pod.Spec.InitContainers) > 0 {
		return nil, fmt.Errorf("vkpodman: initContainers not supported in v1")
	}
	if n := len(pod.Spec.Containers); n != 1 {
		return nil, fmt.Errorf("vkpodman: v1 supports exactly one container per Pod (got %d)", n)
	}
	c := pod.Spec.Containers[0]
	if c.Image == "" {
		return nil, fmt.Errorf("vkpodman: container %q has empty image", c.Name)
	}

	spec := &SpecGenerator{
		Name:     ContainerName(pod),
		Image:    c.Image,
		Hostname: pod.Spec.Hostname,
		WorkDir:  c.WorkingDir,
		Terminal: c.TTY,
		Stdin:    c.Stdin,
		Labels:   buildLabels(pod, c.Name),
	}

	// container.Command in K8s corresponds to OCI ENTRYPOINT; container.Args
	// corresponds to CMD. Libpod's SpecGenerator splits the same way:
	// Entrypoint = ENTRYPOINT override, Command = CMD override. Don't swap
	// these — empty ENTRYPOINT means "use whatever the image baked in".
	if len(c.Command) > 0 {
		spec.Entrypoint = append([]string(nil), c.Command...)
	}
	if len(c.Args) > 0 {
		spec.Command = append([]string(nil), c.Args...)
	}

	if len(c.Env) > 0 {
		env, err := buildEnv(c.Env)
		if err != nil {
			return nil, err
		}
		spec.Env = env
	}
	if len(c.EnvFrom) > 0 {
		return nil, fmt.Errorf("vkpodman: envFrom (configMap/secret refs) not supported in v1")
	}

	if len(c.VolumeMounts) > 0 {
		mounts, volumes, err := buildMounts(pod, c.VolumeMounts)
		if err != nil {
			return nil, err
		}
		spec.Mounts = mounts
		spec.Volumes = volumes
	}

	if rl := buildResourceLimits(c.Resources); rl != nil {
		spec.ResourceLimits = rl
	}

	if pod.Spec.HostNetwork {
		// HostNetwork inherits the host's interfaces; published ports
		// don't apply because the container shares the host's port
		// space directly.
		spec.NetNS = &Namespace{NSMode: "host"}
	} else if len(c.Ports) > 0 {
		spec.PortMappings = buildPortMappings(c.Ports)
	}

	if rp := podmanRestartPolicy(pod.Spec.RestartPolicy); rp != "" {
		spec.RestartPolicy = rp
	}

	return spec, nil
}

// ContainerName is the deterministic libpod container name we use for
// (pod, container). It's derived from the pod UID rather than the
// pod's namespace/name pair so the deleted-and-recreated-with-same-name
// case gets a fresh container instead of colliding with the old one.
//
// Format: "outpost-<first-8-chars-of-uid>-<container-name>". Short
// enough to read in `podman ps`, unique enough that two pods with the
// same container name on the same host don't collide.
func ContainerName(pod *corev1.Pod) string {
	uidPrefix := strings.ReplaceAll(string(pod.UID), "-", "")
	if len(uidPrefix) > 8 {
		uidPrefix = uidPrefix[:8]
	}
	name := pod.Spec.Containers[0].Name
	if name == "" {
		name = "c0"
	}
	return fmt.Sprintf("outpost-%s-%s", uidPrefix, name)
}

func buildLabels(pod *corev1.Pod, containerName string) map[string]string {
	out := map[string]string{
		ManagedLabel:       "true",
		PodUIDLabel:        string(pod.UID),
		PodNamespaceLabel:  pod.Namespace,
		PodNameLabel:       pod.Name,
		ContainerNameLabel: containerName,
	}
	// Record the resolved host ports as labels so HydratePodPortsFromLabels
	// can re-derive them on daemon restart without re-allocating
	// (which would pick different ports and break running clients).
	for k, v := range LabelsForHostPorts(pod) {
		out[k] = v
	}
	// Forward user labels so the host operator's `podman ps --format
	// '{{.Labels}}'` shows what the workload owner attached. We skip
	// anything in the kubernetes.io/* and outpost.io/* namespaces so a
	// malicious or buggy workload can't shadow our reconcile boundary
	// or claim a fake outpost identity.
	for k, v := range pod.Labels {
		if strings.HasPrefix(k, "outpost.io/") || strings.HasPrefix(k, "kubernetes.io/") {
			continue
		}
		out[k] = v
	}
	return out
}

func buildEnv(envs []corev1.EnvVar) (map[string]string, error) {
	out := make(map[string]string, len(envs))
	for _, e := range envs {
		if e.ValueFrom != nil {
			return nil, fmt.Errorf("vkpodman: env %q uses valueFrom (configMap/secret/fieldRef) — not supported in v1", e.Name)
		}
		out[e.Name] = e.Value
	}
	return out, nil
}

// buildMounts walks each VolumeMount, looks up the matching pod.Spec.Volume
// by name, and emits a libpod Mount. Only HostPath and EmptyDir are
// supported in v1.
//
// HostPath and disk-backed EmptyDir both translate to **libpod named
// volumes**, not host bind mounts. The names are deterministic — see
// hostPathVolumeName / emptyDirVolumeName for the keying. The reason
// is environmental: on macOS, podman runs inside a vfkit/libkrun Linux
// VM, and the VM does not have the host's filesystem mounted in. A
// literal bind of "/tmp/x" or "/Users/qiangli/y" fails with
// "statfs ...: no such file or directory" because that path doesn't
// exist inside the VM. Named volumes live inside the VM's own storage
// (Mountpoint: /var/home/core/.local/share/containers/storage/volumes/...)
// and therefore work uniformly on macOS and Linux. The trade-off: the
// HostPath path string is now a logical identifier rather than a
// literal host directory — operators inspect with `podman volume ls`
// / `podman volume inspect outpost-hp-<hash>` instead of `ls /tmp/...`.
//
// Lifetime: HostPath-backed volumes persist until an operator manually
// removes them — matching the K8s "this is a persistent directory on
// the host" intent. EmptyDir-backed volumes are reaped at DeletePod
// (see RemoveEmptyDirsForPod), matching K8s's per-Pod lifetime guarantee.
//
// hostPath.type is now informational only — there's nothing to mkdir
// since the path isn't a real host path. We still validate that path
// is absolute so an obvious user typo gets a clear error.
//
// kube-api-access-* projected volumes (the SA token + CA bundle k8s
// auto-injects on every pod since 1.21) are SILENTLY SKIPPED rather
// than rejected — workload pods on outposts don't need in-cluster API
// access, and rejecting these would refuse every kubectl-run pod.
func buildMounts(pod *corev1.Pod, vms []corev1.VolumeMount) ([]Mount, []NamedVolume, error) {
	volByName := make(map[string]corev1.Volume, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		volByName[v.Name] = v
	}
	var mounts []Mount
	var namedVols []NamedVolume
	for _, vm := range vms {
		if strings.HasPrefix(vm.Name, "kube-api-access-") {
			continue
		}
		v, ok := volByName[vm.Name]
		if !ok {
			return nil, nil, fmt.Errorf("vkpodman: volumeMount %q references unknown volume", vm.Name)
		}
		switch {
		case v.HostPath != nil:
			src := v.HostPath.Path
			if !filepath.IsAbs(src) {
				return nil, nil, fmt.Errorf("vkpodman: hostPath %q must be absolute", src)
			}
			nv := NamedVolume{
				Name: hostPathVolumeName(pod.Namespace, src),
				Dest: vm.MountPath,
			}
			if vm.ReadOnly {
				nv.Options = append(nv.Options, "ro")
			}
			namedVols = append(namedVols, nv)
		case v.EmptyDir != nil:
			if v.EmptyDir.Medium == corev1.StorageMediumMemory {
				m := Mount{Type: "tmpfs", Destination: vm.MountPath}
				if vm.ReadOnly {
					m.Options = append(m.Options, "ro")
				}
				mounts = append(mounts, m)
				break
			}
			nv := NamedVolume{
				Name: emptyDirVolumeName(string(pod.UID), vm.Name),
				Dest: vm.MountPath,
			}
			if vm.ReadOnly {
				nv.Options = append(nv.Options, "ro")
			}
			namedVols = append(namedVols, nv)
		default:
			return nil, nil, fmt.Errorf("vkpodman: volume %q has unsupported type in v1 (only hostPath and emptyDir)", vm.Name)
		}
	}
	return mounts, namedVols, nil
}

// buildResourceLimits maps K8s container resource limits to libpod's
// cgroup-shaped fields. K8s "requests" don't map (libpod has no concept
// of soft reservation); the cluster scheduler uses them for placement
// and we drop them on the floor here.
func buildResourceLimits(rr corev1.ResourceRequirements) *ResourceLimits {
	if len(rr.Limits) == 0 {
		return nil
	}
	out := &ResourceLimits{}
	if cpu, ok := rr.Limits[corev1.ResourceCPU]; ok {
		milli := cpu.MilliValue()
		if milli > 0 {
			// K8s milli-CPU * 100 = µs per period of 100ms — the
			// canonical cgroup v2 cpu.max encoding.
			out.CPU = &CPULimits{Period: 100000, Quota: milli * 100}
		}
	}
	if mem, ok := rr.Limits[corev1.ResourceMemory]; ok {
		v, _ := mem.AsInt64()
		if v > 0 {
			out.Memory = &MemoryLimits{Limit: v}
		}
	}
	if out.CPU == nil && out.Memory == nil {
		return nil
	}
	return out
}

func buildPortMappings(ports []corev1.ContainerPort) []PortMapping {
	out := make([]PortMapping, 0, len(ports))
	for _, p := range ports {
		pm := PortMapping{
			ContainerPort: uint16(p.ContainerPort),
			Protocol:      strings.ToLower(string(p.Protocol)),
			HostIP:        p.HostIP,
		}
		if pm.Protocol == "" {
			pm.Protocol = "tcp"
		}
		if p.HostPort != 0 {
			pm.HostPort = uint16(p.HostPort)
		}
		out = append(out, pm)
	}
	return out
}

// podmanRestartPolicy maps K8s RestartPolicy to libpod's. K8s "Always"
// + "OnFailure" both translate cleanly; "Never" is the libpod default
// (empty string in the spec — no restart_policy field).
func podmanRestartPolicy(rp corev1.RestartPolicy) string {
	switch rp {
	case corev1.RestartPolicyAlways:
		return "always"
	case corev1.RestartPolicyOnFailure:
		return "on-failure"
	default:
		return ""
	}
}
