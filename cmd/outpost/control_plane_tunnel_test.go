package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/runtime"
)

func boolPtr(b bool) *bool { return &b }

// isolateFromPodman hides any container engine from these tests.
//
// Without it they CREATE REAL CONTAINERS on the developer's machine — the
// first run of this suite left a stray `cp-host-control-plane` behind. A unit
// test that reaches the host's container engine is not a unit test, and the
// side effect is invisible until someone lists their containers.
//
// UpServer then fails fast with ErrPodmanNotFound, which is exactly the path
// these tests want: everything this function owns (credentials, persistence,
// the recorded path, clean shutdown) happens around that call, not inside it.
func isolateFromPodman(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// These tests deliberately do NOT assert that a port is listening. The control
// plane is a CONTAINER now — frps runs beside the apiserver inside it, because
// the apiserver reaches a worker's kubelet through a loopback port frps
// publishes and loopback is per network-namespace. Asserting a listener would
// make this suite require a working container engine to say anything at all,
// so it asserts the parts that are this function's own responsibility:
// credentials, persistence, where the kubeconfig will be, and clean shutdown.

func TestStartControlPlaneTunnel_MintsAndPersistsBothCredentials(t *testing.T) {
	isolateFromPodman(t)
	dir := t.TempDir()
	t.Setenv("OUTPOST_CONTROL_PLANE_KUBECONFIG_DIR", filepath.Join(dir, "kube"))
	cfgPath := filepath.Join(dir, "agent.json")
	fc := &conf.FileConfig{
		AgentName: "cp-host",
		Cluster:   &conf.ClusterConfig{ControlPlane: boolPtr(true)},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	startControlPlaneTunnel(gctx, g, fc, cfgPath)

	// A worker needs BOTH. Minting one would leave a control plane that looks
	// configured and refuses every join.
	if len(fc.Cluster.TunnelToken) < 32 {
		t.Errorf("tunnel token not minted: %q", fc.Cluster.TunnelToken)
	}
	if len(fc.Cluster.STCPSecret) < 32 {
		t.Errorf("stcp secret not minted: %q", fc.Cluster.STCPSecret)
	}
	// They must survive a restart, or every boot locks out every worker.
	reloaded, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Cluster == nil ||
		reloaded.Cluster.TunnelToken != fc.Cluster.TunnelToken ||
		reloaded.Cluster.STCPSecret != fc.Cluster.STCPSecret {
		t.Error("credentials were not persisted at boot")
	}

	// The reconcilers need the hosted plane's kubeconfig. Recording the path
	// we already know beats making the operator configure it twice.
	want := runtime.KubeconfigPath(filepath.Join(dir, "kube"))
	if reloaded.Cluster.ControlPlaneKubeconfig != want {
		t.Errorf("kubeconfig path = %q, want %q", reloaded.Cluster.ControlPlaneKubeconfig, want)
	}

	cancel()
	waited := make(chan error, 1)
	go func() { waited <- g.Wait() }()
	select {
	case <-waited:
	case <-time.After(45 * time.Second):
		t.Fatal("errgroup did not drain after cancel — a self-restart would hang here")
	}
}

// An operator who set the path explicitly owns it; boot must not overwrite.
func TestStartControlPlaneTunnel_KeepsOperatorKubeconfigPath(t *testing.T) {
	isolateFromPodman(t)
	dir := t.TempDir()
	t.Setenv("OUTPOST_CONTROL_PLANE_KUBECONFIG_DIR", filepath.Join(dir, "kube"))
	cfgPath := filepath.Join(dir, "agent.json")
	fc := &conf.FileConfig{
		AgentName: "cp-host",
		Cluster: &conf.ClusterConfig{
			ControlPlane:           boolPtr(true),
			ControlPlaneKubeconfig: "/operator/chose/this.yaml",
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	startControlPlaneTunnel(gctx, g, fc, cfgPath)
	if fc.Cluster.ControlPlaneKubeconfig != "/operator/chose/this.yaml" {
		t.Errorf("clobbered the operator's path: %q", fc.Cluster.ControlPlaneKubeconfig)
	}
	cancel()
	_ = g.Wait()
}

// Nearly every host is not a control plane. None of them should mint cluster
// credentials or create a kubeconfig directory.
func TestStartControlPlaneTunnel_NoOpWhenNotControlPlane(t *testing.T) {
	cases := map[string]*conf.ClusterConfig{
		"no cluster config": nil,
		"not control plane": {},
		"explicitly false":  {ControlPlane: boolPtr(false)},
	}
	for name, cc := range cases {
		t.Run(name, func(t *testing.T) {
			isolateFromPodman(t)
			dir := t.TempDir()
			kubeDir := filepath.Join(dir, "kube")
			t.Setenv("OUTPOST_CONTROL_PLANE_KUBECONFIG_DIR", kubeDir)
			fc := &conf.FileConfig{AgentName: "worker", Cluster: cc}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			g, gctx := errgroup.WithContext(ctx)
			startControlPlaneTunnel(gctx, g, fc, filepath.Join(dir, "agent.json"))

			if cc != nil && (cc.TunnelToken != "" || cc.STCPSecret != "") {
				t.Error("minted cluster credentials on a host that hosts nothing")
			}
			if _, err := os.Stat(kubeDir); err == nil {
				t.Error("created a control-plane kubeconfig dir on a non-control-plane host")
			}
			cancel()
			_ = g.Wait()
		})
	}
}
