package vkpodman

import (
	"context"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestIntegration_PodLifecycle runs vkpodman.Run against a real
// kube-apiserver (envtest) and our existing fake libpod, then asserts
// the full control loop: node registers, Pod create → CreateContainer +
// StartContainer hit libpod, Pod delete → RemoveContainer.
//
// Skipped unless KUBEBUILDER_ASSETS points at envtest binaries (etcd +
// kube-apiserver + kubectl). Install them once with:
//
//	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
//	$(go env GOBIN)/setup-envtest use 1.30.0
//
// then run the test with:
//
//	KUBEBUILDER_ASSETS="$(setup-envtest use 1.30.0 -p path)" \
//	  go test ./internal/agent/vkpodman/ -run TestIntegration -v
//
// We keep the test in the default package (no build tag) so it always
// compiles and so the CI signal for "did this even build against
// envtest" stays loud — the actual execution gates on the env var.
func TestIntegration_PodLifecycle(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set — see test doc-comment for setup")
	}

	// Bring up apiserver + etcd. The Stop call on cleanup tears down
	// both binaries.
	testEnv := &envtest.Environment{}
	kubeCfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	// Fake libpod that captures CreateContainer / Start / Inspect / Remove.
	fake := newFakeLibpod()
	sock := startFakeLibpod(t, fake.handler(t))

	// Run vkpodman in a goroutine; we cancel it at the end of the test
	// so envtest's Stop sees a clean disconnect rather than a leaked
	// watcher.
	runCtx, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(cancelRun)
	runDone := make(chan error, 1)
	go func() {
		runDone <- Run(runCtx, RunOptions{
			NodeName:     "smoke",
			PodmanSocket: sock,
			Kube:         kubeCfg,
		})
	}()

	client, err := kubernetes.NewForConfig(kubeCfg)
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}

	// Step 1: Node registers and reaches Ready. Polls every 100ms so a
	// fast registration shows up immediately; gives the heartbeat loop
	// enough time for its first scheduled tick if registration is slow.
	waitForCondition(t, 90*time.Second, "node smoke Ready", func() bool {
		n, err := client.CoreV1().Nodes().Get(context.Background(), "smoke", metav1.GetOptions{})
		if err != nil {
			t.Logf("node not found yet: %v", err)
			return false
		}
		t.Logf("node smoke: phase=%v conditions=%+v", n.Status.Phase, n.Status.Conditions)
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				return true
			}
		}
		return false
	})

	// Step 2: Create a Pod targeted at the smoke node + tolerating the
	// vk taint (the apiserver doesn't auto-stamp this — the production
	// ValidatingAdmissionPolicy would; in v1 it's the workload's job).
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hello",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "smoke",
			Tolerations: []corev1.Toleration{{
				Key:      "virtual-kubelet.io/provider",
				Operator: corev1.TolerationOpExists,
				Effect:   corev1.TaintEffectNoSchedule,
			}},
			Containers: []corev1.Container{{
				Name:    "main",
				Image:   "docker.io/library/alpine:3.20",
				Command: []string{"sh", "-c", "sleep 9999"},
			}},
		},
	}
	if _, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	// Step 3: vkpodman's CreatePod fires → fake libpod sees a container
	// in the running state.
	waitForCondition(t, 30*time.Second, "container created and running", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, c := range fake.containers {
			if c.State == "running" && c.Labels[PodUIDLabel] != "" {
				return true
			}
		}
		return false
	})

	// Step 4: Delete the pod → vkpodman's DeletePod fires → fake libpod
	// drops the container.
	if err := client.CoreV1().Pods("default").Delete(context.Background(), "hello", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	waitForCondition(t, 30*time.Second, "container removed", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.containers) == 0
	})

	// Step 5: Clean shutdown. Cancel the runner, wait for it to return.
	cancelRun()
	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("runner exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("runner did not return within 10s of cancel")
	}
}

// waitForCondition polls cond every 500ms until it returns true or
// timeout elapses. Fails the test with a useful name on timeout so we
// can tell which step hung when the integration suite is slow.
func waitForCondition(t *testing.T, timeout time.Duration, name string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", name)
}
