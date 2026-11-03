package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/ollama"
	"github.com/qiangli/outpost/internal/agent/vknode"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type testWorkerDKS struct {
	nodes     []string
	tokenFile string
}

func (j *testWorkerDKS) Join(_ context.Context, node string) (*vknode.ScopedCredential, error) {
	j.nodes = append(j.nodes, node)
	return &vknode.ScopedCredential{
		APIURL: "https://dks.example.test", Token: "scoped-token", TokenFile: j.tokenFile,
	}, nil
}

type testWorkerSphere struct{ requests []vknode.SphereJoinRequest }

func (*testWorkerSphere) Identity(string) (string, error) { return "stable-mesh-peer", nil }
func (s *testWorkerSphere) Join(_ context.Context, req vknode.SphereJoinRequest) error {
	s.requests = append(s.requests, req)
	if req.EngineURL == "" {
		return errors.New("engine absent")
	}
	return nil
}

type testReadySphere struct {
	testWorkerSphere
	err error
}

func (s *testReadySphere) Ready(context.Context, vknode.SphereJoinRequest) error { return s.err }

type testWorkerBackend struct {
	vknode.Backend
	pod     *corev1.Pod
	ensures int
}

func (b *testWorkerBackend) List(context.Context) ([]*corev1.Pod, error) {
	return []*corev1.Pod{b.pod}, nil
}
func (b *testWorkerBackend) Status(context.Context, *corev1.Pod) (*corev1.PodStatus, error) {
	return &corev1.PodStatus{Phase: corev1.PodRunning}, nil
}
func (b *testWorkerBackend) Ensure(context.Context, *corev1.Pod) error {
	b.ensures++
	return nil
}

func testWorkerConfig() *conf.FileConfig {
	return &conf.FileConfig{
		AgentName: "worker",
		Cluster: &conf.ClusterConfig{
			APIURL: "https://dks.example.test", Token: "scoped-token",
			Runtimes: conf.ClusterRuntimes{Virtual: []string{conf.ClusterRuntimeVKOllama}},
		},
	}
}

func runWorkerStartup(t *testing.T, fc *conf.FileConfig, deps workerBootstrapJoiners) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var g errgroup.Group
	if err := startClusterRunner(ctx, &g, fc, filepath.Join(t.TempDir(), "agent.json"), nil, nil, nil, deps); err != nil {
		cancel()
		return err
	}
	select {
	case err := <-deps.Attempted:
		cancel()
		if waitErr := g.Wait(); waitErr != nil {
			t.Fatal(waitErr)
		}
		return err
	case <-time.After(5 * time.Second):
		cancel()
		_ = g.Wait()
		t.Fatal("worker enrollment did not attempt bootstrap")
		return nil
	}
}

// The runner must invoke Bootstrap using its existing scoped credential and
// configured backend; absent engine/peer prerequisites leave partial state.
func TestBootstrap_StartClusterRunnerEnrollsConfiguredWorker(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tokenFile, err := vknode.DefaultTokenFilePath()
	if err != nil {
		t.Fatal(err)
	}
	dks := &testWorkerDKS{tokenFile: tokenFile}
	sphere := &testWorkerSphere{}
	attempted := make(chan error, 1)
	err = runWorkerStartup(t, testWorkerConfig(), workerBootstrapJoiners{DKS: dks, Sphere: sphere, Attempted: attempted})
	if err == nil {
		t.Fatal("missing engine advertised complete enrollment")
	}
	cache, err := conf.DefaultCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	st, err := vknode.LoadEnrollState(filepath.Join(cache, "worker-bootstrap"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dks.nodes) != 1 || dks.nodes[0] != "worker-vk-ollama" || len(sphere.requests) != 1 {
		t.Fatalf("startup did not call both joiners: nodes=%v sphere=%v", dks.nodes, sphere.requests)
	}
	if st.TokenFile != tokenFile || st.APIURL != "https://dks.example.test" ||
		!st.Done(vknode.PhaseDKSJoined) || st.Done(vknode.PhaseSphereJoined) || st.WorkerReady() || st.LastError == "" {
		t.Fatalf("partial enrollment or scoped credential was wrong: %+v", st)
	}
}

func TestBootstrap_StartClusterRunnerReusesIdentityAndCredentialOnRestart(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tokenFile, err := vknode.DefaultTokenFilePath()
	if err != nil {
		t.Fatal(err)
	}
	dks := &testWorkerDKS{tokenFile: tokenFile}
	sphere := &testReadySphere{}
	backend := &testWorkerBackend{pod: &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("existing-engine")},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Ports: []corev1.ContainerPort{{HostPort: ollama.BashyManagedPort}}}}},
	}}
	var first *vknode.EnrollState
	for run := 0; run < 2; run++ {
		attempted := make(chan error, 1)
		if err := runWorkerStartup(t, testWorkerConfig(), workerBootstrapJoiners{
			DKS: dks, Sphere: sphere, Backend: backend, Attempted: attempted,
		}); err != nil {
			t.Fatal(err)
		}
		cache, err := conf.DefaultCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		st, err := vknode.LoadEnrollState(filepath.Join(cache, "worker-bootstrap"))
		if err != nil {
			t.Fatal(err)
		}
		if st.NodeName != "worker-vk-ollama" || st.PeerID != "stable-mesh-peer" ||
			st.TokenFile != tokenFile || st.EngineUID != "existing-engine" || !st.WorkerReady() {
			t.Fatalf("run %d did not reuse identities and scoped credential: %+v", run, st)
		}
		if _, err := os.Stat(filepath.Join(cache, "worker-bootstrap", "dks", "token")); !os.IsNotExist(err) {
			t.Fatalf("run %d copied the scoped token: %v", run, err)
		}
		if first != nil && (st.MachineID != first.MachineID || st.NodeName != first.NodeName ||
			st.PeerID != first.PeerID || st.CredentialFingerprint != first.CredentialFingerprint ||
			st.TokenFile != first.TokenFile || st.EngineUID != first.EngineUID) {
			t.Fatalf("restart changed enrollment identity: first=%+v second=%+v", first, st)
		}
		first = st
	}
	if len(dks.nodes) != 1 || len(sphere.requests) != 1 || backend.ensures != 0 {
		t.Fatalf("restart duplicated membership or engine: dks=%d sphere=%d ensure=%d", len(dks.nodes), len(sphere.requests), backend.ensures)
	}
	if sphere.requests[0].NodeName != dks.nodes[0] || sphere.requests[0].PeerID != first.PeerID ||
		sphere.requests[0].MachineID != first.MachineID || sphere.requests[0].EngineURL != ollama.BashyOllamaURL() {
		t.Fatalf("sphere joined a different host or engine: %+v", sphere.requests[0])
	}
	sphere.err = errors.New("pool stopped")
	if err := runWorkerStartup(t, testWorkerConfig(), workerBootstrapJoiners{
		DKS: dks, Sphere: sphere, Backend: backend, Attempted: make(chan error, 1),
	}); err == nil {
		t.Fatal("stopped pool kept worker ready")
	}
	cache, err := conf.DefaultCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	st, err := vknode.LoadEnrollState(filepath.Join(cache, "worker-bootstrap"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Done(vknode.PhaseSphereJoined) || st.WorkerReady() || st.LastError == "" {
		t.Fatalf("lost pool did not persist recoverable partial state: %+v", st)
	}
	if len(dks.nodes) != 1 || len(sphere.requests) != 1 || backend.ensures != 0 {
		t.Fatal("prerequisite failure duplicated enrollment or engine")
	}
}
