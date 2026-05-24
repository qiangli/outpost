// Command outpost-vk is a standalone proof-of-concept runner for the
// vkpodman provider. It joins the cluster at --kubeconfig as a virtual
// node named --node, drives a local podman daemon (--podman-socket,
// auto-detected when empty), and stays running until SIGINT.
//
// This binary is intentionally separate from `outpost`: it lets us
// validate the virtual-kubelet + libpod plumbing end-to-end against a
// real k3s server without touching the existing agent's start path or
// admin UI. The actual lifecycle lives in vkpodman.Run; this binary is
// just flag parsing + signal handling around it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/vkpodman"
)

func main() {
	var (
		kubeconfig string
		nodeName   string
		podmanSock string
	)
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig file (required)")
	flag.StringVar(&nodeName, "node", "", "Node name to register (required)")
	flag.StringVar(&podmanSock, "podman-socket", "", "Override path to the local podman socket (auto-detected when empty)")
	flag.Parse()

	if err := run(kubeconfig, nodeName, podmanSock); err != nil {
		slog.Error("outpost-vk exited", "err", err)
		os.Exit(1)
	}
}

func run(kubeconfig, nodeName, podmanSock string) error {
	if kubeconfig == "" {
		return errors.New("--kubeconfig is required")
	}
	if nodeName == "" {
		return errors.New("--node is required")
	}
	if podmanSock == "" {
		bt := agent.DetectPodman()
		if !bt.Available {
			return fmt.Errorf("podman socket not detected; pass --podman-socket. Tried: %s", bt.Socket)
		}
		podmanSock = bt.Socket
		slog.Info("detected podman socket", "socket", podmanSock)
	}

	kubeCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return vkpodman.Run(ctx, vkpodman.RunOptions{
		NodeName:     nodeName,
		PodmanSocket: podmanSock,
		Kube:         kubeCfg,
	})
}
