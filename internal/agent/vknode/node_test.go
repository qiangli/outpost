package vknode

import (
	"context"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/sysinfo"
	corev1 "k8s.io/api/core/v1"
)

func TestBuildNode_BasicShape(t *testing.T) {
	n := BuildNode("home-mini", map[string]string{"outpost.dhnt.io/gpu": "true"})
	if n.Name != "home-mini" {
		t.Errorf("Name: %q", n.Name)
	}
	if n.Labels[corev1.LabelHostname] != "home-mini" {
		t.Errorf("LabelHostname: %q", n.Labels[corev1.LabelHostname])
	}
	if n.Labels[corev1.LabelArchStable] != runtime.GOARCH {
		t.Errorf("LabelArchStable: %q want %q", n.Labels[corev1.LabelArchStable], runtime.GOARCH)
	}
	if n.Labels["outpost.dhnt.io/gpu"] != "true" {
		t.Errorf("extra label not merged: %+v", n.Labels)
	}
	if n.Labels[NodeHostLabel] != "home-mini" {
		t.Errorf("outpost.dhnt.io/host label missing: %+v", n.Labels)
	}
	// Must carry the virtual-kubelet taint so DaemonSets stay off.
	if len(n.Spec.Taints) != 1 || n.Spec.Taints[0].Key != "virtual-kubelet.io/provider" {
		t.Errorf("missing vk taint: %+v", n.Spec.Taints)
	}
	// Must start Ready so the initial registration sticks.
	var ready *corev1.NodeCondition
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == corev1.NodeReady {
			ready = &n.Status.Conditions[i]
		}
	}
	if ready == nil || ready.Status != corev1.ConditionTrue {
		t.Errorf("Ready condition: %+v", ready)
	}
	if _, ok := n.Status.Capacity[corev1.ResourceCPU]; !ok {
		t.Errorf("Capacity missing CPU: %+v", n.Status.Capacity)
	}
	if _, ok := n.Status.Capacity[corev1.ResourceMemory]; !ok {
		t.Errorf("Capacity missing memory: %+v", n.Status.Capacity)
	}
	if got := n.Status.Capacity[corev1.ResourcePods]; got.Value() != 110 {
		t.Errorf("pods capacity = %s, want 110", got.String())
	}
	assertAllocatableMirrorsCapacity(t, n)
}

func TestBuildNodeFromInfo_AppleSiliconMetalVRAM(t *testing.T) {
	const vram = 32 * 1024 * 1024 * 1024
	n := BuildNodeFromInfo("m3-max", nil, sysinfo.Info{
		OS:            "darwin",
		Arch:          "arm64",
		CPUCount:      14,
		MemTotalBytes: 64 * 1024 * 1024 * 1024,
		GPUs: []sysinfo.GPU{{
			Kind:           "apple-silicon",
			Model:          "Apple M3 Max",
			Count:          1,
			VRAMTotalBytes: vram,
			UnifiedMemory:  true,
		}},
	})

	if got := n.Status.Capacity[corev1.ResourceCPU]; got.Value() != 14 {
		t.Errorf("CPU capacity = %s, want 14", got.String())
	}
	if got := n.Status.Capacity[corev1.ResourceMemory]; got.Value() != 64*1024*1024*1024 {
		t.Errorf("memory capacity = %s, want 64Gi", got.String())
	}
	if got := n.Status.Capacity[resourceMetalVRAM]; got.Value() != vram {
		t.Errorf("metal VRAM capacity = %s, want %d", got.String(), int64(vram))
	}
	if _, ok := n.Status.Capacity[resourceNVIDIAGPU]; ok {
		t.Errorf("unexpected NVIDIA resource: %+v", n.Status.Capacity)
	}
	if n.Labels[labelGPU] != "true" {
		t.Errorf("GPU label missing: %+v", n.Labels)
	}
	if n.Labels[labelGPUKind] != "apple-silicon" {
		t.Errorf("GPU kind label = %q", n.Labels[labelGPUKind])
	}
	if n.Labels[labelGPUModel] != "Apple-M3-Max" {
		t.Errorf("GPU model label = %q", n.Labels[labelGPUModel])
	}
	assertAllocatableMirrorsCapacity(t, n)
}

func TestBuildNodeFromInfo_NVIDIAGPU(t *testing.T) {
	n := BuildNodeFromInfo("rtx-host", nil, sysinfo.Info{
		OS:            "linux",
		Arch:          "amd64",
		CPUCount:      32,
		MemTotalBytes: 128 * 1024 * 1024 * 1024,
		GPUs: []sysinfo.GPU{{
			Kind:           "nvidia",
			Model:          "NVIDIA GeForce RTX 4090",
			Count:          2,
			VRAMTotalBytes: 24 * 1024 * 1024 * 1024,
		}},
	})

	if got := n.Status.Capacity[resourceNVIDIAGPU]; got.Value() != 2 {
		t.Errorf("nvidia GPU capacity = %s, want 2", got.String())
	}
	if _, ok := n.Status.Capacity[resourceMetalVRAM]; ok {
		t.Errorf("unexpected metal VRAM resource: %+v", n.Status.Capacity)
	}
	if n.Labels[labelGPUKind] != "nvidia" {
		t.Errorf("GPU kind label = %q", n.Labels[labelGPUKind])
	}
	if n.Labels[labelGPUModel] != "NVIDIA-GeForce-RTX-4090" {
		t.Errorf("GPU model label = %q", n.Labels[labelGPUModel])
	}
	assertAllocatableMirrorsCapacity(t, n)
}

func TestBuildNodeFromInfo_ExtraLabelsOverrideGenerated(t *testing.T) {
	n := BuildNodeFromInfo("gpu-host", map[string]string{
		labelGPUKind: "manual",
	}, sysinfo.Info{
		CPUCount:      4,
		MemTotalBytes: 8 * 1024 * 1024 * 1024,
		GPUs: []sysinfo.GPU{{
			Kind:  "nvidia",
			Model: "NVIDIA Test GPU",
			Count: 1,
		}},
	})

	if n.Labels[labelGPUKind] != "manual" {
		t.Errorf("extra label did not override generated label: %+v", n.Labels)
	}
}

func TestNodeLocalityLabels(t *testing.T) {
	labels := NodeLocalityLabels("Home LAN!", NodeLocalityTierTP)

	if labels[NodeLocalityLANLabel] != "Home-LAN" {
		t.Errorf("lan group label = %q", labels[NodeLocalityLANLabel])
	}
	if labels[NodeLocalityTierLabel] != NodeLocalityTierTP {
		t.Errorf("tier label = %q", labels[NodeLocalityTierLabel])
	}
}

func TestNodeLocalityLabels_OmitsEmptyValues(t *testing.T) {
	labels := NodeLocalityLabels("...", "")

	if len(labels) != 0 {
		t.Errorf("labels = %+v, want empty", labels)
	}
}

func TestBuildNodeFromInfo_LocalityLabels(t *testing.T) {
	extra := NodeLocalityLabels("rack-a", NodeLocalityTierLAN)
	n := BuildNodeFromInfo("lan-host", extra, sysinfo.Info{
		OS:            "linux",
		Arch:          "amd64",
		CPUCount:      4,
		MemTotalBytes: 8 * 1024 * 1024 * 1024,
	})

	if n.Labels[NodeLocalityLANLabel] != "rack-a" {
		t.Errorf("lan group label missing: %+v", n.Labels)
	}
	if n.Labels[NodeLocalityTierLabel] != NodeLocalityTierLAN {
		t.Errorf("tier label missing: %+v", n.Labels)
	}
}

func assertAllocatableMirrorsCapacity(t *testing.T, n *corev1.Node) {
	t.Helper()
	if len(n.Status.Allocatable) != len(n.Status.Capacity) {
		t.Fatalf("Allocatable len = %d, Capacity len = %d", len(n.Status.Allocatable), len(n.Status.Capacity))
	}
	for name, want := range n.Status.Capacity {
		got, ok := n.Status.Allocatable[name]
		if !ok {
			t.Fatalf("Allocatable missing %s", name)
		}
		if got.Cmp(want) != 0 {
			t.Errorf("Allocatable[%s] = %s, want %s", name, got.String(), want.String())
		}
	}
}

func TestNodeProvider_Ping_DelegatesToClient(t *testing.T) {
	sock := startFakeLibpod(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})
	c, _ := NewClient(sock)
	np := NewNodeProvider(c, BuildNode("test", nil))
	if err := np.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestNodeProvider_NotifyNodeStatus_PushesAtLeastOnce(t *testing.T) {
	sock := startFakeLibpod(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})
	c, _ := NewClient(sock)
	np := NewNodeProvider(c, BuildNode("test", nil))

	// Tighten the heartbeat so the test doesn't take ages.
	np.heartbeat = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var pushes []*corev1.Node
	cb := func(n *corev1.Node) {
		mu.Lock()
		defer mu.Unlock()
		pushes = append(pushes, n)
	}

	done := make(chan struct{})
	go func() {
		np.NotifyNodeStatus(ctx, cb)
		close(done)
	}()

	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(pushes) < 2 {
		t.Fatalf("expected ≥2 pushes (initial + at least one tick), got %d", len(pushes))
	}
	// Each push should carry a fresh heartbeat timestamp on the Ready
	// condition (we mutate node in-place before each cb).
	last := pushes[len(pushes)-1]
	var ready *corev1.NodeCondition
	for i := range last.Status.Conditions {
		if last.Status.Conditions[i].Type == corev1.NodeReady {
			ready = &last.Status.Conditions[i]
		}
	}
	if ready == nil || ready.LastHeartbeatTime.IsZero() {
		t.Errorf("Ready condition has no heartbeat: %+v", ready)
	}
}
