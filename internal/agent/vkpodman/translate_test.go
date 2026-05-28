package vkpodman

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// basePod returns a minimal valid v1.Pod that the table-driven tests
// then specialize. Callers usually take a deep-ish copy (modifying
// Containers[0] is fine because each test rebuilds it).
func basePod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hello",
			Namespace: "user-1",
			UID:       types.UID("11111111-2222-3333-4444-555555555555"),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "main",
				Image: "docker.io/library/alpine:3.20",
			}},
		},
	}
}

func TestBuildSpec_NilPod(t *testing.T) {
	if _, err := BuildSpec(nil); err == nil {
		t.Fatal("expected error on nil pod")
	}
}

func TestBuildSpec_RejectsUnsupportedShapes(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*corev1.Pod)
		want string
	}{
		{
			"init containers",
			func(p *corev1.Pod) {
				p.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "busybox"}}
			},
			"initContainers",
		},
		{
			"zero containers",
			func(p *corev1.Pod) { p.Spec.Containers = nil },
			"exactly one container",
		},
		{
			"two containers",
			func(p *corev1.Pod) {
				p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "side", Image: "alpine"})
			},
			"exactly one container",
		},
		{
			"empty image",
			func(p *corev1.Pod) { p.Spec.Containers[0].Image = "" },
			"empty image",
		},
		{
			"envFrom",
			func(p *corev1.Pod) {
				p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{
					ConfigMapRef: &corev1.ConfigMapEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "cm"},
					},
				}}
			},
			"envFrom",
		},
		{
			"env valueFrom",
			func(p *corev1.Pod) {
				p.Spec.Containers[0].Env = []corev1.EnvVar{{
					Name:      "X",
					ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}},
				}}
			},
			"valueFrom",
		},
		{
			"unknown volume reference",
			func(p *corev1.Pod) {
				p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "missing", MountPath: "/x"}}
			},
			"unknown volume",
		},
		{
			"unsupported volume type",
			func(p *corev1.Pod) {
				p.Spec.Volumes = []corev1.Volume{{
					Name: "cm",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "cm"},
						},
					},
				}}
				p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "cm", MountPath: "/etc/cm"}}
			},
			"unsupported type",
		},
		{
			"relative hostPath",
			func(p *corev1.Pod) {
				p.Spec.Volumes = []corev1.Volume{{
					Name:         "data",
					VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "relative/path"}},
				}}
				p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
			},
			"must be absolute",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := basePod()
			tc.mod(p)
			_, err := BuildSpec(p)
			if err == nil {
				t.Fatalf("expected error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want substring %q", err, tc.want)
			}
		})
	}
}

func TestBuildSpec_Identity(t *testing.T) {
	p := basePod()
	spec, err := BuildSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "outpost-11111111-main" {
		t.Errorf("Name = %q want outpost-11111111-main", spec.Name)
	}
	if spec.Image != "docker.io/library/alpine:3.20" {
		t.Errorf("Image = %q", spec.Image)
	}
	// All four well-known labels stamped.
	for _, k := range []string{ManagedLabel, PodUIDLabel, PodNameLabel, PodNamespaceLabel, ContainerNameLabel} {
		if _, ok := spec.Labels[k]; !ok {
			t.Errorf("label %q missing from spec.Labels=%v", k, spec.Labels)
		}
	}
	if spec.Labels[PodNamespaceLabel] != "user-1" {
		t.Errorf("namespace label: %q", spec.Labels[PodNamespaceLabel])
	}
}

func TestBuildSpec_CommandAndArgs(t *testing.T) {
	p := basePod()
	p.Spec.Containers[0].Command = []string{"/bin/sh"}
	p.Spec.Containers[0].Args = []string{"-c", "echo hi"}
	spec, err := BuildSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	// K8s Command → libpod Entrypoint
	if len(spec.Entrypoint) != 1 || spec.Entrypoint[0] != "/bin/sh" {
		t.Errorf("Entrypoint = %v", spec.Entrypoint)
	}
	// K8s Args → libpod Command
	if len(spec.Command) != 2 || spec.Command[0] != "-c" {
		t.Errorf("Command = %v", spec.Command)
	}
}

func TestBuildSpec_Env(t *testing.T) {
	p := basePod()
	p.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "FOO", Value: "bar"},
		{Name: "EMPTY", Value: ""},
	}
	spec, err := BuildSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Env["FOO"] != "bar" {
		t.Errorf("FOO = %q", spec.Env["FOO"])
	}
	if v, ok := spec.Env["EMPTY"]; !ok || v != "" {
		t.Errorf("EMPTY missing or non-empty: %q ok=%v", v, ok)
	}
}

func TestBuildSpec_HostPathMount(t *testing.T) {
	p := basePod()
	p.Namespace = "user-test"
	p.Spec.Volumes = []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/data"}},
	}}
	p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
		{Name: "data", MountPath: "/data", ReadOnly: true},
	}
	spec, err := BuildSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Mounts) != 0 {
		t.Errorf("HostPath should render via Volumes, not Mounts: %+v", spec.Mounts)
	}
	if len(spec.Volumes) != 1 {
		t.Fatalf("Volumes: %+v", spec.Volumes)
	}
	nv := spec.Volumes[0]
	want := hostPathVolumeName(p.Namespace, "/srv/data")
	if nv.Name != want || nv.Dest != "/data" {
		t.Errorf("named vol: %+v (want name=%s)", nv, want)
	}
	if len(nv.Options) != 1 || nv.Options[0] != "ro" {
		t.Errorf("ro option missing: %+v", nv.Options)
	}
}

func TestBuildSpec_EmptyDirMount_Memory(t *testing.T) {
	p := basePod()
	p.Spec.Volumes = []corev1.Volume{{
		Name: "scratch",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory,
		}},
	}}
	p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: "/tmp/scratch"}}
	spec, err := BuildSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].Type != "tmpfs" || spec.Mounts[0].Destination != "/tmp/scratch" {
		t.Errorf("mount: %+v", spec.Mounts)
	}
	if len(spec.Volumes) != 0 {
		t.Errorf("Memory EmptyDir should not emit a NamedVolume: %+v", spec.Volumes)
	}
}

func TestBuildSpec_EmptyDirMount_DiskBacked(t *testing.T) {
	p := basePod()
	p.Spec.Volumes = []corev1.Volume{{
		Name:         "scratch",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: "/tmp/scratch"}}
	spec, err := BuildSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Mounts) != 0 {
		t.Errorf("EmptyDir should render via Volumes, not Mounts: %+v", spec.Mounts)
	}
	want := emptyDirVolumeName(string(p.UID), "scratch")
	if len(spec.Volumes) != 1 || spec.Volumes[0].Name != want {
		t.Errorf("named vol: %+v (want name=%s)", spec.Volumes, want)
	}
}

func TestBuildSpec_ResourceLimits(t *testing.T) {
	p := basePod()
	p.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	spec, err := BuildSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if spec.ResourceLimits == nil {
		t.Fatal("ResourceLimits nil")
	}
	if spec.ResourceLimits.CPU == nil || spec.ResourceLimits.CPU.Period != 100000 || spec.ResourceLimits.CPU.Quota != 50000 {
		t.Errorf("CPU: %+v", spec.ResourceLimits.CPU)
	}
	if spec.ResourceLimits.Memory == nil || spec.ResourceLimits.Memory.Limit != 256*1024*1024 {
		t.Errorf("Memory: %+v", spec.ResourceLimits.Memory)
	}
}

func TestBuildSpec_LimitsOnly_NoSpec(t *testing.T) {
	p := basePod()
	// Requests with no Limits should NOT produce a ResourceLimits entry
	// (libpod has no concept of requests).
	p.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("250m"),
		},
	}
	spec, err := BuildSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if spec.ResourceLimits != nil {
		t.Errorf("expected nil ResourceLimits when only requests set; got %+v", spec.ResourceLimits)
	}
}

func TestBuildSpec_PortsAndHostNetwork(t *testing.T) {
	t.Run("ports without HostNetwork", func(t *testing.T) {
		p := basePod()
		p.Spec.Containers[0].Ports = []corev1.ContainerPort{
			{ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
			{ContainerPort: 53, HostPort: 1053, Protocol: corev1.ProtocolUDP},
		}
		spec, err := BuildSpec(p)
		if err != nil {
			t.Fatal(err)
		}
		if spec.NetNS != nil {
			t.Errorf("NetNS should be unset for default networking: %+v", spec.NetNS)
		}
		if len(spec.PortMappings) != 2 {
			t.Fatalf("PortMappings: %+v", spec.PortMappings)
		}
		if spec.PortMappings[0].ContainerPort != 8080 || spec.PortMappings[0].Protocol != "tcp" {
			t.Errorf("port[0]: %+v", spec.PortMappings[0])
		}
		if spec.PortMappings[1].HostPort != 1053 || spec.PortMappings[1].Protocol != "udp" {
			t.Errorf("port[1]: %+v", spec.PortMappings[1])
		}
	})
	t.Run("HostNetwork supersedes ports", func(t *testing.T) {
		p := basePod()
		p.Spec.HostNetwork = true
		p.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 80}}
		spec, err := BuildSpec(p)
		if err != nil {
			t.Fatal(err)
		}
		if spec.NetNS == nil || spec.NetNS.NSMode != "host" {
			t.Errorf("NetNS: %+v", spec.NetNS)
		}
		if len(spec.PortMappings) != 0 {
			t.Errorf("PortMappings should be empty under HostNetwork: %+v", spec.PortMappings)
		}
	})
}

func TestBuildSpec_RestartPolicy(t *testing.T) {
	for _, tc := range []struct {
		in   corev1.RestartPolicy
		want string
	}{
		{corev1.RestartPolicyAlways, "always"},
		{corev1.RestartPolicyOnFailure, "on-failure"},
		{corev1.RestartPolicyNever, ""},
		{"", ""}, // unset
	} {
		t.Run(string(tc.in), func(t *testing.T) {
			p := basePod()
			p.Spec.RestartPolicy = tc.in
			spec, err := BuildSpec(p)
			if err != nil {
				t.Fatal(err)
			}
			if spec.RestartPolicy != tc.want {
				t.Errorf("RestartPolicy = %q want %q", spec.RestartPolicy, tc.want)
			}
		})
	}
}

func TestBuildSpec_UserLabels_FilterSystem(t *testing.T) {
	p := basePod()
	p.Labels = map[string]string{
		"app":                   "demo",
		"team":                  "platform",
		"kubernetes.io/part-of": "system", // skipped
		"outpost.io/pod-uid":    "evil",   // attempt to shadow — skipped
		"outpost.io/custom-tag": "hack",   // skipped (all outpost.io/* are reserved)
	}
	spec, err := BuildSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Labels["app"] != "demo" || spec.Labels["team"] != "platform" {
		t.Errorf("user labels not forwarded: %+v", spec.Labels)
	}
	if spec.Labels["kubernetes.io/part-of"] != "" {
		t.Errorf("kubernetes.io/* label leaked: %+v", spec.Labels)
	}
	if spec.Labels[PodUIDLabel] == "evil" {
		t.Errorf("user smuggled outpost.io/pod-uid: %q", spec.Labels[PodUIDLabel])
	}
	if _, present := spec.Labels["outpost.io/custom-tag"]; present {
		t.Errorf("outpost.io/* label leaked: %+v", spec.Labels)
	}
}

func TestContainerName_StableAcrossReconciles(t *testing.T) {
	p := basePod()
	n1 := ContainerName(p)
	n2 := ContainerName(p)
	if n1 != n2 {
		t.Errorf("ContainerName not deterministic: %q vs %q", n1, n2)
	}
	if !strings.HasPrefix(n1, "outpost-") {
		t.Errorf("ContainerName missing outpost- prefix: %q", n1)
	}
}

func TestContainerName_VariesByUID(t *testing.T) {
	p1 := basePod()
	p2 := basePod()
	p2.UID = types.UID("99999999-aaaa-bbbb-cccc-dddddddddddd")
	if ContainerName(p1) == ContainerName(p2) {
		t.Errorf("ContainerName should differ across UIDs but both were %q", ContainerName(p1))
	}
}
