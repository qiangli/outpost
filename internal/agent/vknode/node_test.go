package vknode

import (
	"context"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"

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
	if n.Labels["outpost.dhnt.io/host"] != "home-mini" {
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
