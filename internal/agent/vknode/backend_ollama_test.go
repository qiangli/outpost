package vknode

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestHelperProcess is re-exec'd by the ollama backend as the "native
// process" under test (the classic os/exec helper-process pattern), so
// no real ollama/llama-server binary is needed. Behavior is selected by
// the HELPER_MODE env the test stamps onto the Pod:
//
//	serve — bind 127.0.0.1:$HELPER_PORT and accept forever (becomes Ready)
//	sleep — stay alive without binding anything (alive but never Ready)
//	exit  — exit(0) immediately (the "process is gone" case)
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	switch os.Getenv("HELPER_MODE") {
	case "exit":
		os.Exit(0)
	case "sleep":
		time.Sleep(60 * time.Second)
		os.Exit(0)
	case "serve":
		ln, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("HELPER_PORT"))
		if err != nil {
			os.Exit(1)
		}
		for {
			conn, err := ln.Accept()
			if err != nil {
				os.Exit(0)
			}
			_ = conn.Close()
		}
	default:
		os.Exit(0)
	}
}

func newOllamaTestBackend(t *testing.T) (*ollamaBackend, string) {
	t.Helper()
	dir := t.TempDir()
	be, err := NewOllamaBackend(OllamaConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("NewOllamaBackend: %v", err)
	}
	return be.(*ollamaBackend), dir
}

func TestNativeProcessBackend_DefaultImages(t *testing.T) {
	nativeRaw, err := NewNativeProcessBackend(NativeProcessConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewNativeProcessBackend: %v", err)
	}
	native := nativeRaw.(*nativeProcessBackend)
	if native.image != DefaultNativeProcessImage {
		t.Errorf("native default image = %q, want %q", native.image, DefaultNativeProcessImage)
	}

	ollamaRaw, err := NewOllamaBackend(OllamaConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewOllamaBackend: %v", err)
	}
	ollama := ollamaRaw.(*ollamaBackend)
	if ollama.image != DefaultOllamaImage {
		t.Errorf("ollama default image = %q, want %q", ollama.image, DefaultOllamaImage)
	}
}

// makeHelperPod builds a Pod whose container execs this test binary back
// into TestHelperProcess in the given mode. For "serve" it declares a
// containerPort, allocates a hostPort the same way the Provider would,
// and tells the helper which port to bind. Returns the allocated
// hostPort (0 when none).
func makeHelperPod(t *testing.T, name, uid, mode string) (*corev1.Pod, int32) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	env := []corev1.EnvVar{
		{Name: "GO_WANT_HELPER_PROCESS", Value: "1"},
		{Name: "HELPER_MODE", Value: mode},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "user-1",
			Name:      name,
			UID:       types.UID(uid),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:    "main",
				Image:   DefaultOllamaImage,
				Command: []string{exe},
				Args:    []string{"-test.run=^TestHelperProcess$"},
				Env:     env,
			}},
		},
	}
	// "serve" and "sleep" both declare a port so readiness has something
	// to probe; only "serve" actually binds it.
	if mode == "serve" || mode == "sleep" {
		pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 11434}}
		if _, err := AllocateMissingHostPorts(pod); err != nil {
			t.Fatalf("AllocateMissingHostPorts: %v", err)
		}
		hp := pod.Spec.Containers[0].Ports[0].HostPort
		pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env,
			corev1.EnvVar{Name: "HELPER_PORT", Value: strconv.Itoa(int(hp))})
		return pod, hp
	}
	return pod, 0
}

func waitFor(timeout time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fn()
}

func readRegistryFile(t *testing.T, dir string) map[string]procEntry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "registry.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]procEntry{}
		}
		t.Fatalf("read registry: %v", err)
	}
	m := map[string]procEntry{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("parse registry: %v", err)
		}
	}
	return m
}

func TestOllamaBackend_EnsureStatusReadyDelete(t *testing.T) {
	be, dir := newOllamaTestBackend(t)
	ctx := context.Background()
	pod, hostPort := makeHelperPod(t, "serve-pod", "uid-serve", "serve")
	t.Cleanup(func() { _ = be.Delete(ctx, pod) })

	if err := be.Ensure(ctx, pod); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Registry row must record the process + resolved port.
	reg := readRegistryFile(t, dir)
	e, ok := reg["uid-serve"]
	if !ok {
		t.Fatalf("registry missing entry for pod UID: %+v", reg)
	}
	if e.PID <= 0 {
		t.Errorf("registry entry has no PID: %+v", e)
	}
	if len(e.Ports) != 1 || e.Ports[0].HostPort != hostPort {
		t.Errorf("registry ports = %+v, want hostPort %d", e.Ports, hostPort)
	}

	// Poll until the helper has bound its port and Status flips to Running.
	var status *corev1.PodStatus
	ready := waitFor(5*time.Second, func() bool {
		st, err := be.Status(ctx, pod)
		if err != nil || st == nil {
			return false
		}
		status = st
		return st.Phase == corev1.PodRunning
	})
	if !ready {
		t.Fatalf("pod never became Running; last status = %+v", status)
	}
	if status.HostIP != "127.0.0.1" {
		t.Errorf("HostIP = %q, want 127.0.0.1", status.HostIP)
	}
	if len(status.ContainerStatuses) != 1 || !status.ContainerStatuses[0].Ready {
		t.Errorf("container not Ready: %+v", status.ContainerStatuses)
	}
	if status.ContainerStatuses[0].State.Running == nil {
		t.Errorf("container state not Running: %+v", status.ContainerStatuses[0].State)
	}

	// Delete terminates the process and drops the registry row.
	if err := be.Delete(ctx, pod); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := readRegistryFile(t, dir); len(got) != 0 {
		t.Errorf("registry not cleared after Delete: %+v", got)
	}
	// Status after delete: workload gone → (nil, nil).
	gone := waitFor(3*time.Second, func() bool {
		st, err := be.Status(ctx, pod)
		return err == nil && st == nil
	})
	if !gone {
		t.Errorf("Status should report gone after Delete")
	}
}

func TestOllamaBackend_EnsureIdempotent(t *testing.T) {
	be, dir := newOllamaTestBackend(t)
	ctx := context.Background()
	pod, _ := makeHelperPod(t, "idem-pod", "uid-idem", "serve")
	t.Cleanup(func() { _ = be.Delete(ctx, pod) })

	// Count launches by wrapping the default launcher.
	launches := 0
	orig := be.launch
	be.launch = func(ctx context.Context, spec launchSpec) (int, error) {
		launches++
		return orig(ctx, spec)
	}

	if err := be.Ensure(ctx, pod); err != nil {
		t.Fatalf("Ensure #1: %v", err)
	}
	firstPID := readRegistryFile(t, dir)["uid-idem"].PID

	// Wait for the process to actually be alive so the second Ensure
	// takes the adopt path rather than racing the launch.
	if !waitFor(3*time.Second, func() bool { return be.alive(firstPID) }) {
		t.Fatalf("launched process never became alive (pid=%d)", firstPID)
	}

	if err := be.Ensure(ctx, pod); err != nil {
		t.Fatalf("Ensure #2: %v", err)
	}

	if launches != 1 {
		t.Errorf("launch called %d times, want 1 (second Ensure must adopt)", launches)
	}
	reg := readRegistryFile(t, dir)
	if len(reg) != 1 {
		t.Errorf("registry has %d entries, want 1: %+v", len(reg), reg)
	}
	if reg["uid-idem"].PID != firstPID {
		t.Errorf("PID changed across idempotent Ensure: %d -> %d", firstPID, reg["uid-idem"].PID)
	}
}

func TestOllamaBackend_ListRestartRecovery(t *testing.T) {
	be1, dir := newOllamaTestBackend(t)
	ctx := context.Background()
	pod, hostPort := makeHelperPod(t, "recover-pod", "uid-recover", "serve")

	if err := be1.Ensure(ctx, pod); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	firstPID := readRegistryFile(t, dir)["uid-recover"].PID
	if !waitFor(3*time.Second, func() bool { return be1.alive(firstPID) }) {
		t.Fatalf("process never became alive")
	}

	// Simulate a vknode restart: a brand-new backend over the same data
	// dir. The process from the prior lifetime is still running.
	be2raw, err := NewOllamaBackend(OllamaConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("NewOllamaBackend #2: %v", err)
	}
	be2 := be2raw.(*ollamaBackend)
	t.Cleanup(func() { _ = be2.Delete(ctx, pod) })

	pods, err := be2.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("List returned %d pods, want 1: %+v", len(pods), pods)
	}
	got := pods[0]
	if got.Namespace != "user-1" || got.Name != "recover-pod" || string(got.UID) != "uid-recover" {
		t.Errorf("skeleton identity wrong: %+v", got.ObjectMeta)
	}
	if len(got.Spec.Containers) != 1 {
		t.Fatalf("skeleton has %d containers", len(got.Spec.Containers))
	}
	ports := got.Spec.Containers[0].Ports
	if len(ports) != 1 || ports[0].HostPort != hostPort {
		t.Errorf("skeleton ports = %+v, want hostPort %d", ports, hostPort)
	}

	// The new backend should also be able to report status by adopting
	// the surviving process.
	ready := waitFor(5*time.Second, func() bool {
		st, err := be2.Status(ctx, got)
		return err == nil && st != nil && st.Phase == corev1.PodRunning
	})
	if !ready {
		t.Errorf("recovered backend did not see the process as Running")
	}
}

func TestOllamaBackend_HydratePorts(t *testing.T) {
	be, _ := newOllamaTestBackend(t)
	ctx := context.Background()
	pod, hostPort := makeHelperPod(t, "hydrate-pod", "uid-hydrate", "serve")
	t.Cleanup(func() { _ = be.Delete(ctx, pod) })

	if err := be.Ensure(ctx, pod); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// A fresh apiserver-side pod (same UID) never saw the allocated
	// hostPort — HydratePorts must fill it in from the registry.
	fresh := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "user-1",
			Name:      "hydrate-pod",
			UID:       types.UID("uid-hydrate"),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "main",
				Image: DefaultOllamaImage,
				Ports: []corev1.ContainerPort{{ContainerPort: 11434}},
			}},
		},
	}
	if err := be.HydratePorts(ctx, fresh); err != nil {
		t.Fatalf("HydratePorts: %v", err)
	}
	got := fresh.Spec.Containers[0].Ports[0].HostPort
	if got != hostPort {
		t.Errorf("HydratePorts set HostPort=%d, want %d", got, hostPort)
	}

	// A missing registry row is a no-op, not an error.
	absent := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID("uid-nope")}}
	if err := be.HydratePorts(ctx, absent); err != nil {
		t.Errorf("HydratePorts on unknown pod should be a no-op: %v", err)
	}
}

func TestOllamaBackend_GoneProcessNilStatus(t *testing.T) {
	be, _ := newOllamaTestBackend(t)
	ctx := context.Background()
	pod, _ := makeHelperPod(t, "exit-pod", "uid-exit", "exit")
	t.Cleanup(func() { _ = be.Delete(ctx, pod) })

	if err := be.Ensure(ctx, pod); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// The helper exits immediately; Status must converge to (nil, nil)
	// even though the registry row still exists.
	gone := waitFor(5*time.Second, func() bool {
		st, err := be.Status(ctx, pod)
		return err == nil && st == nil
	})
	if !gone {
		t.Fatalf("Status never reported the exited process as gone")
	}
}

func TestOllamaBackend_PendingBeforeReady(t *testing.T) {
	be, _ := newOllamaTestBackend(t)
	ctx := context.Background()
	// "sleep" stays alive but never binds its declared port, so it's
	// running-but-not-ready: Pending / ContainerCreating.
	pod, _ := makeHelperPod(t, "pending-pod", "uid-pending", "sleep")
	t.Cleanup(func() { _ = be.Delete(ctx, pod) })

	if err := be.Ensure(ctx, pod); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	st, err := be.Status(ctx, pod)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st == nil {
		t.Fatalf("Status nil for a live-but-not-ready process")
	}
	if st.Phase != corev1.PodPending {
		t.Errorf("phase = %q, want Pending", st.Phase)
	}
	if len(st.ContainerStatuses) != 1 || st.ContainerStatuses[0].Ready {
		t.Errorf("container should be not-Ready: %+v", st.ContainerStatuses)
	}
	w := st.ContainerStatuses[0].State.Waiting
	if w == nil || w.Reason != "ContainerCreating" {
		t.Errorf("waiting reason = %+v, want ContainerCreating", w)
	}
}
