// Command outpost-vk is a standalone proof-of-concept runner for the
// vkpodman provider. It joins the cluster at --kubeconfig as a virtual
// node named --node, and stays running until SIGINT.
//
// Two backends are supported (--backend):
//
//   - podman (default) — drives a local podman daemon (--podman-socket,
//     auto-detected when empty). Pods become libpod containers.
//   - native — realizes Pods as host processes (--native-data, default
//     ~/.cache/outpost/native-process). Pods run as detached host
//     processes, directly on the host OS.
//   - ollama — legacy alias for the native-process backend, defaulting
//     its data dir to ~/.cache/outpost/ollama and its marker image to
//     dhnt.io/ollama.
//
// This binary is intentionally separate from `outpost`: it lets us
// validate the virtual-kubelet plumbing end-to-end against a real k3s
// server without touching the existing agent's start path or admin UI.
// The actual lifecycle lives in vknode.Run; this binary is just flag
// parsing + signal handling around it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/vknode"
)

func main() {
	var (
		kubeconfig  string
		nodeName    string
		podmanSock  string
		backendName string
		nativeData  string
		ollamaData  string
		allowAnyNS  bool
	)
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig file (required)")
	flag.StringVar(&nodeName, "node", "", "Node name to register (required)")
	flag.StringVar(&podmanSock, "podman-socket", "", "Override path to the local podman socket (auto-detected when empty; podman backend only)")
	flag.StringVar(&backendName, "backend", "podman", "Workload substrate: podman (default), native, or ollama")
	flag.StringVar(&nativeData, "native-data", "", "Data directory for the native-process backend (default ~/.cache/outpost/native-process)")
	flag.StringVar(&ollamaData, "ollama-data", "", "Data directory for the legacy ollama backend (default ~/.cache/outpost/ollama)")
	flag.BoolVar(&allowAnyNS, "allow-any-namespace", false, "Dev/PoC only: allow native backend pods from any namespace")
	flag.Parse()

	if err := run(kubeconfig, nodeName, podmanSock, backendName, nativeData, ollamaData, allowAnyNS); err != nil {
		slog.Error("outpost-vk exited", "err", err)
		os.Exit(1)
	}
}

func run(kubeconfig, nodeName, podmanSock, backendName, nativeData, ollamaData string, allowAnyNS bool) error {
	if kubeconfig == "" {
		return errors.New("--kubeconfig is required")
	}
	if nodeName == "" {
		return errors.New("--node is required")
	}

	opts := vknode.RunOptions{
		NodeName: nodeName,
	}

	switch backendName {
	case "podman":
		if podmanSock == "" {
			bt := agent.DetectPodman()
			if !bt.Available {
				return fmt.Errorf("podman socket not detected; pass --podman-socket. Tried: %s", bt.Socket)
			}
			podmanSock = bt.Socket
			slog.Info("detected podman socket", "socket", podmanSock)
		}
		opts.PodmanSocket = podmanSock
	case "native", "process", "native-process":
		if !allowAnyNS {
			return errors.New("--backend=native requires --allow-any-namespace for this standalone PoC runner")
		}
		if nativeData == "" {
			cacheDir, err := conf.DefaultCacheDir()
			if err != nil {
				return fmt.Errorf("native backend: determine default data dir: %w", err)
			}
			nativeData = filepath.Join(cacheDir, "native-process")
		}
		slog.Info("native-process backend", "data_dir", nativeData)
		be, err := vknode.NewNativeProcessBackend(vknode.NativeProcessConfig{DataDir: nativeData})
		if err != nil {
			return fmt.Errorf("native backend: %w", err)
		}
		opts.Backend = be
		opts.AllowAnyNamespace = true
	case "ollama":
		if !allowAnyNS {
			return errors.New("--backend=ollama requires --allow-any-namespace for this standalone PoC runner")
		}
		if ollamaData == "" {
			cacheDir, err := conf.DefaultCacheDir()
			if err != nil {
				return fmt.Errorf("ollama backend: determine default data dir: %w", err)
			}
			ollamaData = filepath.Join(cacheDir, "ollama")
		}
		slog.Info("ollama backend", "data_dir", ollamaData)
		be, err := vknode.NewOllamaBackend(vknode.OllamaConfig{DataDir: ollamaData})
		if err != nil {
			return fmt.Errorf("ollama backend: %w", err)
		}
		opts.Backend = be
		opts.AllowAnyNamespace = true
	default:
		return fmt.Errorf("unknown --backend %q; must be podman, native, or ollama", backendName)
	}

	kubeCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	opts.Kube = kubeCfg

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return vknode.Run(ctx, opts)
}
