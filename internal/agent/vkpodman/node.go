package vkpodman

import (
	"context"
	"maps"
	"runtime"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeProvider implements virtual-kubelet's node.NodeProvider on top of
// the libpod client. Ping is the lightweight heartbeat used to mark the
// node Ready/NotReady; NotifyNodeStatus pushes the slow-path status
// updates (capacity, conditions) that we want the apiserver to see when
// they change.
type NodeProvider struct {
	client    *Client
	node      *corev1.Node
	heartbeat time.Duration
}

// NewNodeProvider returns a NodeProvider sharing the given libpod
// client. node is the initial Node object — typically built with
// BuildNode below; NotifyNodeStatus mutates it in place (touching the
// Ready condition's LastHeartbeatTime) before each push.
func NewNodeProvider(c *Client, node *corev1.Node) *NodeProvider {
	return &NodeProvider{
		client:    c,
		node:      node,
		heartbeat: staticHeartbeat,
	}
}

// Ping is the lightweight liveness check virtual-kubelet calls
// periodically to drive the node lease. We pass it straight through to
// /libpod/_ping — a non-2xx response or any transport error fails the
// heartbeat and marks the node NotReady.
func (np *NodeProvider) Ping(ctx context.Context) error {
	return np.client.Ping(ctx)
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
		now := metav1.NewTime(time.Now())
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

// BuildNode constructs the initial *corev1.Node the NodeController
// registers with the apiserver. nodeName is what `kubectl get nodes`
// will show; labels merge with the well-known kubernetes.io/*
// platform labels.
//
// Capacity/Allocatable use runtime.NumCPU() and the system memory
// detected via the local libpod /info endpoint (TODO — for v1 we
// report a placeholder so registration succeeds; the scheduler can
// still place workloads based on nodeSelector rather than resource
// requests until we wire the real numbers).
func BuildNode(nodeName string, extraLabels map[string]string) *corev1.Node {
	labels := map[string]string{
		corev1.LabelHostname:   nodeName,
		corev1.LabelOSStable:   runtime.GOOS,
		corev1.LabelArchStable: runtime.GOARCH,
		"type":                 "virtual-kubelet",
		"outpost.dhnt.io/host": nodeName,
	}
	maps.Copy(labels, extraLabels)

	// Placeholder capacity. Real values land when we wire libpod
	// /info into NewNodeProvider — until then, we advertise enough
	// for the scheduler to consider the node and let nodeSelector be
	// the practical placement signal.
	cpuQty := *resource.NewQuantity(int64(runtime.NumCPU()), resource.DecimalSI)
	memQty := *resource.NewQuantity(8*1024*1024*1024, resource.BinarySI) // 8 GiB placeholder

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
				OperatingSystem: runtime.GOOS,
				Architecture:    runtime.GOARCH,
				KubeletVersion:  "v0.1.0-vkpodman",
			},
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    cpuQty,
				corev1.ResourceMemory: memQty,
				corev1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    cpuQty,
				corev1.ResourceMemory: memQty,
				corev1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
			},
			Conditions: []corev1.NodeCondition{{
				Type:               corev1.NodeReady,
				Status:             corev1.ConditionTrue,
				Reason:             "KubeletReady",
				Message:            "outpost vkpodman is ready",
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
