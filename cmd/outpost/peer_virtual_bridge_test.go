package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func TestPeerVirtualAPIURLUsesDedicatedRuntimeBridge(t *testing.T) {
	cc := &conf.ClusterConfig{
		JoinEndpoint: "peer.example:7000",
		K8sAPIPort:   6443,
		Runtimes: conf.ClusterRuntimes{
			Agent:   true,
			Virtual: []string{conf.ClusterRuntimeVKNative},
		},
	}
	if got, want := peerVirtualAPIURL(cc), "https://127.0.0.1:16444"; got != want {
		t.Fatalf("peerVirtualAPIURL() = %q, want %q", got, want)
	}
}

func TestPeerVirtualAPIURLPreservesCloudAndVirtualOnlyPaths(t *testing.T) {
	for _, cc := range []*conf.ClusterConfig{
		{K8sAPIPort: 7443, Runtimes: conf.ClusterRuntimes{Agent: true}},
		{JoinEndpoint: "peer.example:7000", K8sAPIPort: 7443,
			Runtimes: conf.ClusterRuntimes{Virtual: []string{conf.ClusterRuntimeVKNative}}},
	} {
		if got, want := peerVirtualAPIURL(cc), "https://127.0.0.1:7443"; got != want {
			t.Fatalf("peerVirtualAPIURL() = %q, want %q", got, want)
		}
	}
}

func TestRunVirtualNodeWithRetryRetriesFailureAndUnexpectedExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	runVirtualNodeWithRetry(ctx, "node-vk-native", "vk-native", time.Millisecond, 2*time.Millisecond,
		func(context.Context) error {
			calls++
			switch calls {
			case 1:
				return errors.New("apiserver unavailable")
			case 2:
				return nil
			default:
				cancel()
				return context.Canceled
			}
		})
	if calls != 3 {
		t.Fatalf("runner calls = %d, want 3", calls)
	}
}
