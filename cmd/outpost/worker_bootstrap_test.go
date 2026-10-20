package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/vknode"
	"golang.org/x/sync/errgroup"
)

type testWorkerSphere struct{ joins int }

func (*testWorkerSphere) Identity(string) (string, error) { return "stable-mesh-peer", nil }
func (s *testWorkerSphere) Join(context.Context, vknode.SphereJoinRequest) error {
	s.joins++
	return nil
}

// The cluster runner already has the scoped credential and the configured
// backend. Starting it must pass those same inputs to worker enrollment.
func TestBootstrap_StartClusterRunnerEnrollsConfiguredWorker(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fc := &conf.FileConfig{
		AgentName: "worker",
		Cluster: &conf.ClusterConfig{
			APIURL: "https://dks.example.test", Token: "scoped-token",
			Runtimes: conf.ClusterRuntimes{Virtual: []string{conf.ClusterRuntimeVKOllama}},
		},
	}
	var g errgroup.Group
	if err := startClusterRunner(ctx, &g, fc, filepath.Join(t.TempDir(), "agent.json"), nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = g.Wait()
	cache, err := conf.DefaultCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	statePath := vknode.EnrollStatePath(filepath.Join(cache, "worker-bootstrap"))
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("startClusterRunner did not invoke worker bootstrap with its scoped kubeconfig and vk-ollama backend: %v", err)
	}
	st, err := vknode.LoadEnrollState(filepath.Dir(statePath))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Done(vknode.PhaseDKSJoined) || st.Done(vknode.PhaseSphereJoined) || st.WorkerReady() {
		t.Fatalf("missing peer/engine advertised readiness: %+v", st)
	}
	if st.LastError == "" {
		t.Fatal("missing peer/engine prerequisite did not record a recoverable failure")
	}
}

func TestBootstrap_StartClusterRunnerReusesIdentityAndCredentialOnRestart(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fc := &conf.FileConfig{
		AgentName: "worker",
		Cluster: &conf.ClusterConfig{
			APIURL: "https://dks.example.test", Token: "scoped-token",
			Runtimes: conf.ClusterRuntimes{Virtual: []string{conf.ClusterRuntimeVKOllama}},
		},
	}
	joiner := &testWorkerSphere{}
	var first *vknode.EnrollState
	for run := 0; run < 2; run++ {
		ctx, cancel := context.WithCancel(context.Background())
		var g errgroup.Group
		err := startClusterRunner(ctx, &g, fc, filepath.Join(t.TempDir(), "agent.json"), nil, nil, nil,
			workerBootstrapJoiners{Sphere: joiner})
		cancel()
		_ = g.Wait()
		if err != nil {
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
		liveToken, err := vknode.DefaultTokenFilePath()
		if err != nil {
			t.Fatal(err)
		}
		if st.NodeName != "worker-vk-ollama" || st.PeerID != "stable-mesh-peer" ||
			st.TokenFile != liveToken || !st.WorkerReady() {
			t.Fatalf("run %d did not reuse node, peer and live scoped credential: %+v", run, st)
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
	if joiner.joins != 1 {
		t.Fatalf("sphere joined %d times, want one", joiner.joins)
	}
}
