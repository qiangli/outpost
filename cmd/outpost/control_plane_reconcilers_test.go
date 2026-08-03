package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/nodeaddr"
)

// fastControlPlaneBackoff shrinks the kubeconfig-wait backoff so these tests
// exercise the retry loop without sleeping for real seconds.
func fastControlPlaneBackoff(t *testing.T) {
	t.Helper()
	first, max := controlPlaneClientFirstBackoff, controlPlaneClientMaxBackoff
	controlPlaneClientFirstBackoff = time.Millisecond
	controlPlaneClientMaxBackoff = 5 * time.Millisecond
	t.Cleanup(func() {
		controlPlaneClientFirstBackoff, controlPlaneClientMaxBackoff = first, max
	})
}

// stubControlPlaneClient replaces the kubeconfig→clientset step with build.
func stubControlPlaneClient(t *testing.T, build func(path string) (kubernetes.Interface, error)) {
	t.Helper()
	prev := buildControlPlaneClient
	buildControlPlaneClient = build
	t.Cleanup(func() { buildControlPlaneClient = prev })
}

// nodeOnlyInternalIP is a worker in exactly the state Dragon's cluster was
// found in: Ready, an overlay InternalIP, and NO ExternalIP — so metrics-server
// (pinned to --kubelet-preferred-address-types=ExternalIP) has nothing to dial
// and reports "no address matched types [ExternalIP]".
func nodeOnlyInternalIP(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "100.64.0.3"},
				{Type: corev1.NodeHostName, Address: name},
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// externalIPOf returns the node's ExternalIP, or "" when it has none.
func externalIPOf(t *testing.T, cs kubernetes.Interface, name string) string {
	t.Helper()
	n, err := cs.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node %s: %v", name, err)
	}
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeExternalIP {
			return a.Address
		}
	}
	return ""
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// THE REGRESSION THIS FILE EXISTS FOR.
//
// The control-plane kubeconfig is authored by the control-plane CONTAINER
// (server-entrypoint.sh writes <dir>/k3s.yaml only after k3s is up), while
// startControlPlaneReconcilers runs at the instant that container's supervisor
// is merely spawned. The old code resolved the kubeconfig exactly once and
// returned on failure — so on any cold boot the file was not there yet, the
// nodeaddr reconciler never started for the daemon's whole lifetime, and no
// Node ever received the loopback ExternalIP the apiserver dials kubelets
// through. Nodes went Ready, pods scheduled, and every `kubectl top nodes`
// failed.
//
// This test reproduces that ordering exactly: the kubeconfig does not exist
// when startControlPlaneReconcilers is called, and appears afterwards. It fails
// against the one-shot implementation and passes against the retrying one.
func TestStartControlPlaneReconcilers_WaitsForKubeconfigWrittenAfterStart(t *testing.T) {
	fastControlPlaneBackoff(t)
	nodeaddr.ResetStatus()
	t.Setenv("OUTPOST_NODEGC", "off") // keep this test to the addressing story

	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "k3s.yaml")

	cs := k8sfake.NewSimpleClientset(nodeOnlyInternalIP("novidesign-4c7adeb6"))
	stubControlPlaneClient(t, func(path string) (kubernetes.Interface, error) {
		// Exactly what clientcmd does with a path that is not there yet.
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		return cs, nil
	})

	on := true
	fc := &conf.FileConfig{
		AgentName: "dragon",
		Cluster: &conf.ClusterConfig{
			ControlPlane:           &on,
			ControlPlaneKubeconfig: kubeconfig,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)

	startControlPlaneReconcilers(gctx, g, fc)

	// The container writes its kubeconfig only now — after startup already ran.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(kubeconfig, []byte("placeholder"), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	waitFor(t, 5*time.Second, "nodeaddr to publish an ExternalIP", func() bool {
		return externalIPOf(t, cs, "novidesign-4c7adeb6") != ""
	})

	got := externalIPOf(t, cs, "novidesign-4c7adeb6")
	want := nodeaddr.LoopbackForNode("novidesign-4c7adeb6", map[string]string{})
	if got != want {
		t.Errorf("ExternalIP = %q, want the reconciler-derived %q", got, want)
	}
	// Uniqueness is the reason the address is derived rather than fixed: a
	// shared 127.0.0.1 collapses k3s's routing trie onto one arbitrary node.
	if got == "127.0.0.1" {
		t.Errorf("ExternalIP must be node-unique, not the shared tunnel bind address")
	}

	cancel()
	_ = g.Wait()
}

// The reconciler set must actually contain the addressing reconciler. This
// fails if nodeaddr is ever dropped from runControlPlaneReconcilers — the
// omission that is invisible to every config-level test.
func TestRunControlPlaneReconcilers_StartsNodeAddrReconciler(t *testing.T) {
	nodeaddr.ResetStatus()
	t.Setenv("OUTPOST_NODEGC", "off")

	cs := k8sfake.NewSimpleClientset(nodeOnlyInternalIP("worker-a"))
	fc := &conf.FileConfig{AgentName: "dragon"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runControlPlaneReconcilers(ctx, fc, cs, nil)
	}()

	waitFor(t, 5*time.Second, "an ExternalIP patch", func() bool {
		return externalIPOf(t, cs, "worker-a") != ""
	})
	// And the reconciler must report itself running, so `outpost status` can
	// answer "is nodeaddr running?" without grepping the daemon log.
	if s, ok := nodeaddr.LastStatus(); !ok || !s.Started {
		t.Errorf("nodeaddr.LastStatus() = %+v, ok=%v; want a started reconciler", s, ok)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runControlPlaneReconcilers did not stop on context cancellation")
	}
}

// A path that is merely not there YET must be waited for; a path nobody ever
// recorded is a real misconfiguration and must decline loudly instead of
// spinning forever on a name that will never exist.
func TestStartControlPlaneReconcilers_DeclinesWithoutKubeconfigPath(t *testing.T) {
	buf := captureSlog(t)
	nodeaddr.ResetStatus()
	stubControlPlaneClient(t, func(string) (kubernetes.Interface, error) {
		t.Fatal("must not attempt to build a client without a configured path")
		return nil, nil
	})

	on := true
	fc := &conf.FileConfig{
		AgentName: "dragon",
		Cluster:   &conf.ClusterConfig{ControlPlane: &on},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	startControlPlaneReconcilers(gctx, g, fc)
	cancel()
	_ = g.Wait()

	if out := buf.String(); !strings.Contains(out, "control_plane_kubeconfig") {
		t.Errorf("declining for a missing path must say which key to set; got:\n%s", out)
	}
}

// A host that does not host the plane must not run cluster-wide controllers:
// one copy per node would have every node racing to patch every other node.
func TestStartControlPlaneReconcilers_SkipsWhenNotHostingThePlane(t *testing.T) {
	nodeaddr.ResetStatus()
	stubControlPlaneClient(t, func(string) (kubernetes.Interface, error) {
		t.Fatal("a non-control-plane host must not build a control-plane client")
		return nil, nil
	})

	fc := &conf.FileConfig{
		AgentName: "worker",
		Cluster:   &conf.ClusterConfig{ControlPlaneKubeconfig: "/tmp/does-not-matter"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	startControlPlaneReconcilers(gctx, g, fc)
	cancel()
	_ = g.Wait()
}

// The wait must be bounded only by ctx: a transient failure is retried, not
// treated as terminal, and cancellation is the single way out.
func TestAwaitControlPlaneClient_RetriesThenSucceeds(t *testing.T) {
	fastControlPlaneBackoff(t)

	attempts := 0
	want := k8sfake.NewSimpleClientset()
	stubControlPlaneClient(t, func(string) (kubernetes.Interface, error) {
		attempts++
		if attempts < 4 {
			return nil, errors.New("no such file or directory")
		}
		return want, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := awaitControlPlaneClient(ctx, "/some/k3s.yaml", nil)
	if err != nil {
		t.Fatalf("awaitControlPlaneClient: %v", err)
	}
	if got != kubernetes.Interface(want) {
		t.Errorf("returned a different client than the builder produced")
	}
	if attempts < 4 {
		t.Errorf("attempts = %d, want the loop to have retried", attempts)
	}
}

func TestAwaitControlPlaneClient_StopsOnContextCancel(t *testing.T) {
	fastControlPlaneBackoff(t)
	stubControlPlaneClient(t, func(string) (kubernetes.Interface, error) {
		return nil, errors.New("never ready")
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := awaitControlPlaneClient(ctx, "/some/k3s.yaml", nil)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("awaitControlPlaneClient ignored context cancellation")
	}
}
