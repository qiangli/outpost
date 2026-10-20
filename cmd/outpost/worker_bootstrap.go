package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/machineid"
	"github.com/qiangli/outpost/internal/agent/mesh"
	"github.com/qiangli/outpost/internal/agent/ollama"
	"github.com/qiangli/outpost/internal/agent/peerplane"
	"github.com/qiangli/outpost/internal/agent/vknode"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
)

type workerBootstrapInput struct {
	Config       *conf.FileConfig
	Cluster      *conf.ClusterConfig
	NodeName     string
	Backend      vknode.Backend
	Kube         *rest.Config
	TokenFile    string
	CacheBase    string
	CloudboxBase string
	PeerService  *peerplane.Service
}

// Supplying joiners is a test seam. Production uses the already materialized
// scoped credential and the existing peer/model services.
type workerBootstrapJoiners struct {
	DKS    vknode.DKSJoiner
	Sphere vknode.SphereJoiner
	Pool   *ollamaCapability
}

type existingDKSJoiner struct{ credential vknode.ScopedCredential }

func (j existingDKSJoiner) Join(context.Context, string) (*vknode.ScopedCredential, error) {
	return &j.credential, nil
}

type workerSphereJoiner struct {
	host  string
	base  string
	token string
	peer  *peerplane.Service
	pool  *ollamaCapability
}

func (j workerSphereJoiner) Identity(_ string) (string, error) {
	// The mesh host reads this same key; enrollment does not create a second
	// sphere identity under its private state directory.
	return mesh.PeerIDForDir("")
}

func (j workerSphereJoiner) Join(ctx context.Context, req vknode.SphereJoinRequest) error {
	if j.peer == nil || j.pool == nil {
		return errors.New("peer service or model pool is not configured")
	}
	if req.EngineURL == "" || j.pool.Target() != req.EngineURL {
		return fmt.Errorf("model pool is not bound to worker engine %q", req.EngineURL)
	}
	if status, ok := j.pool.Status(); !ok || !status.Watcher.Running {
		return errors.New("model pool watcher is not running")
	}
	candidates := j.peer.Candidates()
	if len(candidates) == 0 {
		return errors.New("peer service has no reachability candidates")
	}
	return (&peerplane.Client{BaseURL: j.base, Token: j.token}).Announce(ctx, j.host, req.PeerID, candidates, nil)
}

func bootstrapWorker(ctx context.Context, in workerBootstrapInput, deps workerBootstrapJoiners) error {
	if in.Config == nil || in.Cluster == nil || in.Kube == nil || in.Backend == nil {
		return errors.New("worker bootstrap missing cluster backend or credential")
	}
	if in.Cluster.Token == "" || in.TokenFile == "" {
		return errors.New("worker bootstrap requires a scoped bearer credential")
	}
	mid, err := machineid.Get()
	if err != nil {
		slog.Warn("cluster worker bootstrap: machine ID unavailable; using hostname", "err", err)
	}
	dks := deps.DKS
	if dks == nil {
		dks = existingDKSJoiner{vknode.ScopedCredential{
			APIURL: in.Kube.Host, Token: in.Cluster.Token, CA: in.Kube.CAData,
			TokenFile: in.TokenFile, CAFile: in.Kube.CAFile,
		}}
	}
	sphere := deps.Sphere
	if sphere == nil {
		sphere = workerSphereJoiner{
			host: in.Config.AgentName, base: in.CloudboxBase,
			token: in.Config.AccessToken, peer: in.PeerService, pool: deps.Pool,
		}
	}
	pairing := strings.TrimSpace(in.CloudboxBase) + "|" + in.Config.AgentName + "|" + in.Kube.Host
	if in.Cluster.JoinsPeerPlane() {
		pairing = "peer|" + in.Kube.Host + "|" + in.Config.AgentName
	}
	opts := vknode.BootstrapOptions{
		MachineID: mid, Hostname: in.Config.AgentName,
		NodeName: in.NodeName, Backend: conf.ClusterRuntimeVKOllama,
		PairingID: pairing, StateDir: filepath.Join(in.CacheBase, "worker-bootstrap"),
		DKS: dks, Sphere: sphere,
	}
	// The normal vknode controller owns process creation. Read its existing
	// backend registry and bind the sphere watcher to that one live engine.
	pods, err := in.Backend.List(ctx)
	if err != nil {
		return fmt.Errorf("list existing engine: %w", err)
	}
	for _, pod := range pods {
		if pod == nil || pod.UID == "" || !podUsesManagedEnginePort(pod) {
			continue
		}
		status, err := in.Backend.Status(ctx, pod)
		if err != nil || status == nil || status.Phase != corev1.PodRunning {
			continue
		}
		opts.ExistingEngineUID = string(pod.UID)
		opts.ExistingEngineURL = ollama.BashyOllamaURL()
		break
	}
	_, err = vknode.Bootstrap(ctx, opts)
	return err
}

func podUsesManagedEnginePort(pod *corev1.Pod) bool {
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			if port.HostPort == ollama.BashyManagedPort {
				return true
			}
		}
	}
	return false
}

func retryWorkerBootstrap(ctx context.Context, in workerBootstrapInput, deps workerBootstrapJoiners) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := bootstrapWorker(ctx, in, deps); err == nil {
				return
			} else {
				slog.Warn("cluster worker bootstrap: still partial", "err", err)
			}
		}
	}
}
