package main

import (
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/vknode"
)

func TestStandaloneRuntimeIdentityVocabulary(t *testing.T) {
	for _, backend := range []string{
		conf.ClusterRuntimeVKPodman,
		conf.ClusterRuntimeVKNative,
		conf.ClusterRuntimeVKOllama,
	} {
		if !conf.ValidVirtualRuntime(backend) {
			t.Fatalf("standalone backend %q is not a valid virtual runtime", backend)
		}
		labels := standaloneNodeLabels(backend, "fallback-host")
		if labels[vknode.NodeHostLabel] == "" ||
			labels["outpost.dhnt.io/runtime"] != "virtual" ||
			labels["outpost.dhnt.io/backend"] != backend {
			t.Fatalf("standalone %s identity = %#v", backend, labels)
		}
	}
}
