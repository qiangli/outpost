package vknode

import (
	"context"
	"maps"
	"math"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qiangli/outpost/internal/agent/sysinfo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	labelGPU      = "outpost.dhnt.io/gpu"
	labelGPUKind  = "outpost.dhnt.io/gpu-kind"
	labelGPUModel = "outpost.dhnt.io/gpu-model"

	// resourceVRAM is the vendor-neutral GPU-memory resource, in bytes.
	// It is THE quantity to request when a workload needs to fit in GPU
	// memory — a model shard, an image batch — regardless of who made
	// the card.
	//
	// The division of labour here is the k8s-native one, and it is worth
	// stating because the earlier metal-vram design collapsed it:
	//
	//   - HOW MUCH is a consumable → an extended RESOURCE. The scheduler
	//     must subtract it, so two shards can't both claim the same GiB.
	//   - WHICH KIND is an attribute → a LABEL (outpost.dhnt.io/gpu-kind,
	//     already stamped by addGPULabels). Vendor is not consumable;
	//     placing a CUDA build on an Apple GPU is an affinity question,
	//     not an arithmetic one.
	//
	// Baking the vendor into the resource name conflated the two and made
	// the resource unrequestable on three quarters of the fleet: a
	// manifest asking for dhnt.io/metal-vram is unschedulable on every
	// NVIDIA/AMD/Intel host no matter how much VRAM it has.
	resourceVRAM = corev1.ResourceName("dhnt.io/vram")

	// resourceMetalVRAM is the Apple-only predecessor of resourceVRAM,
	// still advertised on apple-silicon nodes so manifests written
	// against it keep scheduling. DEPRECATED — new manifests want
	// resourceVRAM plus a gpu-kind affinity. Remove once no in-tree
	// scaffold emits it and the fleet has rolled past this release.
	resourceMetalVRAM = corev1.ResourceName("dhnt.io/metal-vram")

	resourceNVIDIAGPU = corev1.ResourceName("nvidia.com/gpu")
)

// NodeProvider implements virtual-kubelet's node.NodeProvider on top of
// the libpod client. Ping is the lightweight heartbeat used to mark the
// node Ready/NotReady; NotifyNodeStatus pushes the slow-path status
// updates (capacity, conditions) that we want the apiserver to see when
// they change.
type NodeProvider struct {
	client        *Client
	node          *corev1.Node
	pinger        func(context.Context) error
	heartbeat     time.Duration
	hostLoad      func() HostLoad
	requested     func() corev1.ResourceList
	loadFreshness time.Duration
}

// HostLoad is the current physical-host pressure used to keep a virtual
// node's Allocatable resources honest. Negative percentages mean unknown.
// At is mandatory: stale measurements fail open to the node's static capacity
// rather than stranding work because a sampler stopped.
type HostLoad struct {
	At           time.Time
	CPUPercent   float64
	MemAvailFrac float64
}

// NewNodeProvider returns a NodeProvider sharing the given libpod
// client. node is the initial Node object — typically built with
// BuildNode below; NotifyNodeStatus mutates it in place (touching the
// Ready condition's LastHeartbeatTime) before each push.
//
// When c is non-nil, Ping delegates to c.Ping (the libpod /libpod/_ping
// endpoint). When c is nil (native backends), Ping returns nil by
// default — callers can override with SetPinger.
func NewNodeProvider(c *Client, node *corev1.Node) *NodeProvider {
	np := &NodeProvider{
		client:    c,
		node:      node,
		pinger:    func(_ context.Context) error { return nil },
		heartbeat: staticHeartbeat,
		// Three missed samples is long enough to absorb scheduling jitter but
		// short enough not to advertise yesterday's pressure after a sampler
		// failure. The production sampler runs every 30 seconds.
		loadFreshness: 3 * staticHeartbeat,
	}
	if c != nil {
		np.pinger = c.Ping
	}
	return np
}

// SetResourceAccounting enables current-utilization-aware Allocatable
// reporting. load describes the whole physical host; requested returns this
// virtual node's active pod requests. Adding the latter back is essential:
// the Kubernetes scheduler subtracts bound pod requests from Allocatable, so
// publishing host-free resources alone would count this node's pods twice.
//
// Multiple vk venues on one host still have a simultaneous-placement race:
// Kubernetes does not have a native cross-Node shared-resource primitive.
// Once a sibling venue starts consuming resources, however, its utilization
// is visible to every venue at the next sample and reduces their headroom.
func (np *NodeProvider) SetResourceAccounting(load func() HostLoad, requested func() corev1.ResourceList) {
	np.hostLoad = load
	np.requested = requested
}

// SetPinger replaces the health-check function used by Ping. When the
// pinger returns an error the node is marked NotReady. The zero value
// (when client is nil and SetPinger is never called) is an always-
// healthy pinger — callers of native backends can swap in a custom
// probe (e.g. "is the ollama process reachable?") via this seam.
func (np *NodeProvider) SetPinger(fn func(context.Context) error) {
	np.pinger = fn
}

// Ping is the lightweight liveness check virtual-kubelet calls
// periodically to drive the node lease. When backed by a podman client
// it passes through to /libpod/_ping; for native backends it calls
// the configured pinger (always-healthy by default).
func (np *NodeProvider) Ping(ctx context.Context) error {
	return np.pinger(ctx)
}

// NotifyNodeStatus starts an asynchronous heartbeat loop that pushes
// a fresh Node status to cb every staticHeartbeat. **It must not block
// the caller** — virtual-kubelet's NodeController.Run calls this inline
// during setup and won't proceed to ensureNode (where the apiserver
// POST happens) until we return. The loop runs in a goroutine and
// exits when ctx is canceled.
func (np *NodeProvider) NotifyNodeStatus(ctx context.Context, cb func(*corev1.Node)) {
	go np.runStatusLoop(ctx, cb)
}

func (np *NodeProvider) runStatusLoop(ctx context.Context, cb func(*corev1.Node)) {
	tick := time.NewTicker(np.heartbeat)
	defer tick.Stop()

	push := func() {
		nowTime := time.Now()
		now := metav1.NewTime(nowTime)
		np.refreshAllocatable(nowTime)
		for i := range np.node.Status.Conditions {
			c := &np.node.Status.Conditions[i]
			c.LastHeartbeatTime = now
			if c.Type == corev1.NodeReady {
				c.Status = corev1.ConditionTrue
				c.LastTransitionTime = now
			}
		}
		cb(np.node.DeepCopy())
	}
	push()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			push()
		}
	}
}

func (np *NodeProvider) refreshAllocatable(now time.Time) {
	// Fail open when accounting is disabled, has not sampled yet, or stopped
	// sampling. A dead optional profiler must never permanently strand pods.
	if np.hostLoad == nil {
		np.node.Status.Allocatable = maps.Clone(np.node.Status.Capacity)
		return
	}
	load := np.hostLoad()
	age := now.Sub(load.At)
	if load.At.IsZero() || age < -np.loadFreshness || age > np.loadFreshness {
		np.node.Status.Allocatable = maps.Clone(np.node.Status.Capacity)
		return
	}

	alloc := maps.Clone(np.node.Status.Capacity)
	var requested corev1.ResourceList
	if np.requested != nil {
		requested = np.requested()
	}
	if load.CPUPercent >= 0 && load.CPUPercent <= 100 {
		total := np.node.Status.Capacity.Cpu().MilliValue()
		free := int64(math.Floor(float64(total) * (100 - load.CPUPercent) / 100))
		alloc[corev1.ResourceCPU] = *resource.NewMilliQuantity(
			availableWithRequests(free, requested.Cpu().MilliValue(), total), resource.DecimalSI)
	}
	if load.MemAvailFrac >= 0 && load.MemAvailFrac <= 1 {
		total := np.node.Status.Capacity.Memory().Value()
		free := int64(math.Floor(float64(total) * load.MemAvailFrac))
		alloc[corev1.ResourceMemory] = *resource.NewQuantity(
			availableWithRequests(free, requested.Memory().Value(), total), resource.BinarySI)
	}
	np.node.Status.Allocatable = alloc
}

func availableWithRequests(free, requested, capacity int64) int64 {
	if free < 0 {
		free = 0
	}
	if free >= capacity || requested >= capacity-free {
		return capacity
	}
	if requested <= 0 {
		return free
	}
	return free + requested
}

// BuildNode constructs the initial *corev1.Node the NodeController
// registers with the apiserver. nodeName is what `kubectl get nodes`
// will show; labels merge with the well-known kubernetes.io/*
// platform labels.
//
// Capacity/Allocatable come from the local sysinfo probe.
func BuildNode(nodeName string, extraLabels map[string]string) *corev1.Node {
	return BuildNodeFromInfo(nodeName, extraLabels, sysinfo.Collect(""))
}

// BuildNodeFromInfo constructs the initial Node object from already-collected
// host capability info. It is kept separate from BuildNode so tests and future
// callers can feed peer-provided sysinfo without probing the local machine.
func BuildNodeFromInfo(nodeName string, extraLabels map[string]string, info sysinfo.Info) *corev1.Node {
	osName := firstNonEmpty(info.OS, runtime.GOOS)
	arch := firstNonEmpty(info.Arch, runtime.GOARCH)
	labels := map[string]string{
		corev1.LabelHostname:   nodeName,
		corev1.LabelOSStable:   osName,
		corev1.LabelArchStable: arch,
		"type":                 "virtual-kubelet",
		NodeHostLabel:          nodeName,
	}
	addGPULabels(labels, info.GPUs)
	maps.Copy(labels, extraLabels)

	capacity := capacityFromInfo(info)

	now := metav1.NewTime(time.Now())
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: labels,
		},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{
				// Standard virtual-kubelet taint — keeps system pods
				// (kube-proxy DaemonSets, CSI drivers, etc.) off of
				// our nodes. Workload pods opt in via toleration.
				{
					Key:    "virtual-kubelet.io/provider",
					Value:  "outpost",
					Effect: corev1.TaintEffectNoSchedule,
				},
			},
		},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{
				OperatingSystem: osName,
				Architecture:    arch,
				KubeletVersion:  "v0.1.0-vknode",
			},
			Capacity:    capacity,
			Allocatable: maps.Clone(capacity),
			Conditions: []corev1.NodeCondition{{
				Type:               corev1.NodeReady,
				Status:             corev1.ConditionTrue,
				Reason:             "KubeletReady",
				Message:            "outpost vknode is ready",
				LastHeartbeatTime:  now,
				LastTransitionTime: now,
			}},
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeHostName, Address: nodeName},
			},
			DaemonEndpoints: corev1.NodeDaemonEndpoints{
				KubeletEndpoint: corev1.DaemonEndpoint{Port: 0},
			},
		},
	}
}

// LocalityTierForMeasured maps a peer-plane MEASURED tier value (the
// ground-truth RTT classification — "tp"/"lan"/"wan"/"unreached") to the
// Node's locality-tier label value. tp/lan/wan pass through unchanged;
// an unreachable, empty, or unknown input collapses to
// NodeLocalityTierRemote so a host whose only links are relay-only still
// carries an explicit tier instead of a silently-missing label.
//
// This is the seam that replaces the previously-stubbed tier: callers
// feed peerplane.Service.SelfTier() through here and into
// NodeLocalityLabels, so the Node's tier reflects what was measured
// rather than a hardcoded guess. Kept as a plain string map so vknode
// stays decoupled from the peerplane package.
func LocalityTierForMeasured(measured string) string {
	switch measured {
	case NodeLocalityTierTP:
		return NodeLocalityTierTP
	case NodeLocalityTierLAN:
		return NodeLocalityTierLAN
	case NodeLocalityTierWAN:
		return NodeLocalityTierWAN
	default:
		return NodeLocalityTierRemote
	}
}

func capacityFromInfo(info sysinfo.Info) corev1.ResourceList {
	cpuCount := info.CPUCount
	if cpuCount <= 0 {
		cpuCount = runtime.NumCPU()
	}

	capacity := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(cpuCount), resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(uint64ToInt64(info.MemTotalBytes), resource.BinarySI),
		corev1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
	}

	// VRAM is summed across EVERY vendor — the resource describes bytes
	// of GPU memory, and bytes do not care who made the card. A device
	// reporting 0 contributes nothing: sysinfo uses 0 to mean "could not
	// measure" (see wmiVRAMLooksCapped), and advertising an unmeasured
	// card as some default would be worse than not advertising it.
	var vram uint64
	var metalVRAM uint64
	var nvidiaGPUs int64
	for _, gpu := range info.GPUs {
		vram += gpu.VRAMTotalBytes
		switch gpu.Kind {
		case sysinfo.GPUKindApple:
			metalVRAM += gpu.VRAMTotalBytes
		case sysinfo.GPUKindNVIDIA:
			if gpu.Count > 0 {
				nvidiaGPUs += int64(gpu.Count)
			}
		}
	}
	if vram > 0 {
		capacity[resourceVRAM] = *resource.NewQuantity(uint64ToInt64(vram), resource.BinarySI)
	}
	// Deprecated alias, apple-silicon only — keeps pre-existing manifests
	// schedulable on the nodes they were written for. See resourceMetalVRAM.
	if metalVRAM > 0 {
		capacity[resourceMetalVRAM] = *resource.NewQuantity(uint64ToInt64(metalVRAM), resource.BinarySI)
	}
	if nvidiaGPUs > 0 {
		capacity[resourceNVIDIAGPU] = *resource.NewQuantity(nvidiaGPUs, resource.DecimalSI)
	}
	return capacity
}

func addGPULabels(labels map[string]string, gpus []sysinfo.GPU) {
	for _, gpu := range gpus {
		if gpu.Kind == "" && gpu.Model == "" {
			continue
		}
		labels[labelGPU] = "true"
		if kind := sanitizeLabelValue(gpu.Kind); kind != "" {
			labels[labelGPUKind] = kind
		}
		if model := sanitizeLabelValue(gpu.Model); model != "" {
			labels[labelGPUModel] = model
		}
		return
	}
}

func sanitizeLabelValue(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_.")
	for len(out) > 63 {
		_, size := utf8.DecodeLastRuneInString(out)
		out = out[:len(out)-size]
		out = strings.TrimRight(out, "-_.")
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func uint64ToInt64(n uint64) int64 {
	const maxInt64 = uint64(^uint64(0) >> 1)
	if n > maxInt64 {
		return int64(maxInt64)
	}
	return int64(n)
}
