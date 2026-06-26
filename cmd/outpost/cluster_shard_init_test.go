package main

import (
	"strings"
	"testing"
)

func renderShard(t *testing.T, in shardInput) string {
	t.Helper()
	data, err := buildShardVars(in)
	if err != nil {
		t.Fatalf("buildShardVars: %v", err)
	}
	var sb strings.Builder
	if err := renderShardManifest(&sb, data); err != nil {
		t.Fatalf("renderShardManifest: %v", err)
	}
	return sb.String()
}

func baseShardInput() shardInput {
	return shardInput{
		name:       "llama70b",
		image:      "ghcr.io/ggml-org/llama.cpp:full",
		model:      "/models/llama-70b-q4.gguf",
		workerIPs:  "192.168.1.21, 192.168.1.22",
		rpcPort:    50052,
		port:       8080,
		lanGroup:   "home",
		tier:       "lan",
		leaderVRAM: "24Gi",
		workerVRAM: "24Gi",
		topology:   "lws",
	}
}

func TestShardManifestLWS(t *testing.T) {
	out := renderShard(t, baseShardInput())

	mustContain := []string{
		"kind: LeaderWorkerSet",
		"command: [\"llama-server\"]",
		"command: [\"rpc-server\"]",
		// workers addressed by host IP, baked into --rpc
		"192.168.1.21:50052,192.168.1.22:50052",
		// nodeAffinity on lan-group + tier
		"key: outpost.dhnt.io/lan-group",
		"key: outpost.dhnt.io/tier",
		// metal-vram requests
		"dhnt.io/metal-vram: 24Gi",
		// 1 leader + 2 workers
		"size: 3",
		// native host process: no pod net
		"hostNetwork: true",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("LWS manifest missing %q\n---\n%s", s, out)
		}
	}
}

func TestShardManifestDeployment(t *testing.T) {
	in := baseShardInput()
	in.topology = "deployment"
	out := renderShard(t, in)

	mustContain := []string{
		"kind: Deployment",
		"kind: Service",
		"clusterIP: None", // headless
		"name: llama70b-leader",
		"name: llama70b-worker",
		"replicas: 2", // one per worker IP
		"192.168.1.21:50052,192.168.1.22:50052",
		"key: outpost.dhnt.io/lan-group",
		"key: outpost.dhnt.io/tier",
		"dhnt.io/metal-vram: 24Gi",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("deployment manifest missing %q\n---\n%s", s, out)
		}
	}
	if strings.Contains(out, "kind: LeaderWorkerSet") {
		t.Errorf("deployment topology should not emit LeaderWorkerSet\n%s", out)
	}
}

func TestShardManifestAffinityPresentBothPods(t *testing.T) {
	// Both leader and worker pods must carry the identical placement
	// contract (lan-group + tier nodeAffinity).
	out := renderShard(t, baseShardInput())
	if got := strings.Count(out, "key: outpost.dhnt.io/lan-group"); got != 2 {
		t.Errorf("expected lan-group nodeAffinity on leader and worker (2), got %d", got)
	}
	if got := strings.Count(out, "key: outpost.dhnt.io/tier"); got != 2 {
		t.Errorf("expected tier nodeAffinity on leader and worker (2), got %d", got)
	}
}

func TestShardVarsValidation(t *testing.T) {
	cases := map[string]func(*shardInput){
		"missing name":      func(in *shardInput) { in.name = "" },
		"missing model":     func(in *shardInput) { in.model = "" },
		"missing image":     func(in *shardInput) { in.image = "" },
		"missing lan-group": func(in *shardInput) { in.lanGroup = "" },
		"missing workers":   func(in *shardInput) { in.workerIPs = "  ,  " },
		"bad rpc-port":      func(in *shardInput) { in.rpcPort = 0 },
		"bad port":          func(in *shardInput) { in.port = 70000 },
		"bad topology":      func(in *shardInput) { in.topology = "statefulset" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := baseShardInput()
			mutate(&in)
			if _, err := buildShardVars(in); err == nil {
				t.Errorf("expected error for %s, got nil", name)
			}
		})
	}
}

func TestShardTierDefaults(t *testing.T) {
	in := baseShardInput()
	in.tier = ""
	data, err := buildShardVars(in)
	if err != nil {
		t.Fatalf("buildShardVars: %v", err)
	}
	if data.Tier != "lan" {
		t.Errorf("empty tier should default to lan, got %q", data.Tier)
	}
}

// TestShardInitCmdBuilds ensures the cobra command wires up without panic
// and is reachable from the cluster command tree.
func TestShardInitCmdBuilds(t *testing.T) {
	cmd := clusterShardInitCmd()
	if cmd.Use != "shard-init" {
		t.Fatalf("unexpected Use: %q", cmd.Use)
	}
	parent := clusterCmd()
	var found bool
	for _, c := range parent.Commands() {
		if c.Use == "shard-init" {
			found = true
			break
		}
	}
	if !found {
		t.Error("shard-init not registered under cluster command")
	}
}
