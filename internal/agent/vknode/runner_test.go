package vknode

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// fakeBackend is defined in provider_test.go — reuse it here for
// runner-level tests that need a Backend without a real substrate.

// TestGuard_ContainsPanic locks in the load-bearing property behind the
// v0.12.32 outage: a panic inside a virtual-kubelet controller goroutine
// (the updateNodeStatus shutdown-race nil deref) must be converted into
// an error, not allowed to unwind past the errgroup and crash the whole
// outpost daemon. If this test starts crashing the process instead of
// failing, the recover() was lost.
func TestGuard_ContainsPanic(t *testing.T) {
	err := guard("node controller", func() error {
		// nilNode() hides the nil from the static analyzer; the deref
		// still panics at runtime, same shape as node.go:599.
		_ = nilNode().ResourceVersion
		return nil
	})
	if err == nil {
		t.Fatal("expected guard to convert the panic into an error")
	}
	if !strings.Contains(err.Error(), "node controller panicked") {
		t.Errorf("expected panic to be labeled with the goroutine name, got: %v", err)
	}
}

// nilNode returns a nil *corev1.Node without the static analyzer being
// able to prove it — used only to reproduce the runtime nil deref.
func nilNode() *corev1.Node {
	m := map[string]*corev1.Node{}
	return m["absent"]
}

func TestGuard_PassesThroughNormalReturn(t *testing.T) {
	if err := guard("x", func() error { return nil }); err != nil {
		t.Errorf("guard should pass a nil return through, got: %v", err)
	}
	sentinel := context.Canceled
	if err := guard("x", func() error { return sentinel }); err != sentinel {
		t.Errorf("guard should pass a real error through unchanged, got: %v", err)
	}
}

func TestRun_PodmanSocketRequiredWhenNoBackend(t *testing.T) {
	err := Run(context.Background(), RunOptions{NodeName: "x"})
	if err == nil {
		t.Fatal("expected error when no Backend and no PodmanSocket")
	}
	if !strings.Contains(err.Error(), "PodmanSocket required") {
		t.Errorf("expected PodmanSocket error, got: %v", err)
	}
}

func TestRun_BackendFieldSkipsPodmanSocketCheck(t *testing.T) {
	err := Run(context.Background(), RunOptions{
		NodeName: "x",
		Backend:  &fakeBackend{},
	})
	if err == nil {
		t.Fatal("expected error (will fail further down at Kube check)")
	}
	if strings.Contains(err.Error(), "PodmanSocket") {
		t.Errorf("PodmanSocket error should not fire when Backend is set: %v", err)
	}
	if !strings.Contains(err.Error(), "Kube") {
		t.Errorf("expected Kube error, got: %v", err)
	}
}

func TestRun_BackendProviderRequiresExplicitAccess(t *testing.T) {
	// NewProviderWithBackend sets requireExplicitAccess=true — verify
	// that a native (fake) backend rejects pods when Access is nil.
	b := &fakeBackend{}
	p := NewProviderWithBackend(b)
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "test"}},
		},
	}
	pod.Namespace = "any-ns"
	pod.Name = "test-pod"

	err := p.CreatePod(context.Background(), pod)
	if err == nil {
		t.Fatal("native backend with nil Access should reject")
	}
	if !strings.Contains(err.Error(), "native backend requires explicit namespace access") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRun_BackendProviderAllowAnyNamespace(t *testing.T) {
	b := &fakeBackend{}
	p := NewProviderWithBackend(b)
	p.SetRequireExplicitAccess(false)
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "test"}},
		},
	}
	pod.Namespace = "any-ns"
	pod.Name = "test-pod"

	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("native backend with explicit dev bypass should accept nil Access: %v", err)
	}
}

func TestRun_PodmanBackendDefaultPath_RequiresSocket(t *testing.T) {
	// The default (podman) path requires PodmanSocket.
	err := Run(context.Background(), RunOptions{
		NodeName:     "x",
		PodmanSocket: "",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "PodmanSocket required") {
		t.Errorf("expected PodmanSocket error, got: %v", err)
	}
}
