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
		gpuKind:    "nvidia",
		backend:    "vk-ollama",
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
		// vendor-neutral VRAM requests
		"dhnt.io/vram: 24Gi",
		// vendor is a separate, label-based axis
		"key: outpost.dhnt.io/gpu-kind",
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
		"dhnt.io/vram: 24Gi",
		"key: outpost.dhnt.io/gpu-kind",
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
		"missing name":    func(in *shardInput) { in.name = "" },
		"missing model":   func(in *shardInput) { in.model = "" },
		"missing image":   func(in *shardInput) { in.image = "" },
		"missing workers": func(in *shardInput) { in.workerIPs = "  ,  " },
		"bad rpc-port":    func(in *shardInput) { in.rpcPort = 0 },
		"bad port":        func(in *shardInput) { in.port = 70000 },
		"bad topology":    func(in *shardInput) { in.topology = "statefulset" },
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

// TestShardTierNoDefault: an unset tier stays unset. The old default of
// "lan" silently injected a requiredDuringScheduling term the operator
// never asked for — and since a node only carries outpost.dhnt.io/tier
// when cloudbox measured its locality, that default could pin the group
// to a label the target nodes lacked. Absent means unconstrained.
func TestShardTierNoDefault(t *testing.T) {
	in := baseShardInput()
	in.tier = ""
	data, err := buildShardVars(in)
	if err != nil {
		t.Fatalf("buildShardVars: %v", err)
	}
	if data.Tier != "" {
		t.Errorf("empty tier must stay empty, got %q", data.Tier)
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

// TestShardManifestOmitsUnsetAffinityTerms is the regression guard for the
// defect that made the scaffold unusable: --lan-group was required and
// always emitted, but outpost only stamps outpost.dhnt.io/lan-group on
// nodes with cloudbox-issued locality data — which on a real fleet was NO
// node. A requiredDuringScheduling term naming an absent label matches
// nothing, so every rendered manifest was unschedulable everywhere.
func TestShardManifestOmitsUnsetAffinityTerms(t *testing.T) {
	in := baseShardInput()
	in.lanGroup = ""
	in.tier = ""
	out := renderShard(t, in)

	if strings.Contains(out, "outpost.dhnt.io/lan-group") {
		t.Errorf("unset lan-group must not be emitted\n---\n%s", out)
	}
	if strings.Contains(out, "outpost.dhnt.io/tier") {
		t.Errorf("unset tier must not be emitted\n---\n%s", out)
	}
	// The term that WAS set still lands, on both pods.
	if got := strings.Count(out, "key: outpost.dhnt.io/gpu-kind"); got != 2 {
		t.Errorf("expected gpu-kind affinity on leader and worker (2), got %d\n---\n%s", got, out)
	}
}

// TestShardManifestNoAffinityAtAll: with no placement constraint the
// affinity block must be absent entirely rather than rendered empty —
// an "affinity:" key with no nodeAffinity under it is invalid YAML shape
// for the pod spec.
func TestShardManifestNoAffinityAtAll(t *testing.T) {
	in := baseShardInput()
	in.lanGroup = ""
	in.tier = ""
	in.gpuKind = ""
	in.backend = ""
	out := renderShard(t, in)

	if strings.Contains(out, "affinity:") {
		t.Errorf("no placement terms set: affinity block must be omitted\n---\n%s", out)
	}
	if !strings.Contains(out, "dhnt.io/vram: 24Gi") {
		t.Errorf("VRAM request must survive regardless of affinity\n---\n%s", out)
	}
}

// TestShardManifestLANGroupNoLongerRequired: the flag used to be a hard
// validation error. Dropping it is deliberate — see the doc on
// nodeAffinityBlock.
func TestShardManifestLANGroupNoLongerRequired(t *testing.T) {
	in := baseShardInput()
	in.lanGroup = ""
	if _, err := buildShardVars(in); err != nil {
		t.Fatalf("lan-group must be optional, got error: %v", err)
	}
}

// TestShardManifestCarriesVKToleration: vknode taints its Node
// virtual-kubelet.io/provider=outpost:NoSchedule, so a shard pod without
// this toleration is refused by the only nodes that can run it. Both the
// leader and every worker need it, in both topologies.
func TestShardManifestCarriesVKToleration(t *testing.T) {
	for _, topo := range []string{"lws", "deployment"} {
		t.Run(topo, func(t *testing.T) {
			in := baseShardInput()
			in.topology = topo
			out := renderShard(t, in)
			if got := strings.Count(out, "key: virtual-kubelet.io/provider"); got != 2 {
				t.Errorf("expected the vk toleration on leader and worker (2), got %d\n---\n%s", got, out)
			}
		})
	}
}

// TestShardManifestVRAMHasEqualLimit guards the constraint that made the
// scaffold un-appliable regardless of hardware: dhnt.io/vram is an
// EXTENDED resource, and Kubernetes refuses a pod naming one in requests
// without an equal limit ("Limit must be set for non overcommitable
// resources"). The object was rejected by the API server, so the
// scheduler never saw it.
func TestShardManifestVRAMHasEqualLimit(t *testing.T) {
	for _, topo := range []string{"lws", "deployment"} {
		t.Run(topo, func(t *testing.T) {
			in := baseShardInput()
			in.topology = topo
			in.leaderVRAM = "24Gi"
			in.workerVRAM = "16Gi"
			out := renderShard(t, in)

			// One requests + one limits per container, leader and worker.
			if got := strings.Count(out, "dhnt.io/vram: 24Gi"); got != 2 {
				t.Errorf("leader VRAM should appear in both requests and limits (2), got %d\n---\n%s", got, out)
			}
			if got := strings.Count(out, "dhnt.io/vram: 16Gi"); got != 2 {
				t.Errorf("worker VRAM should appear in both requests and limits (2), got %d\n---\n%s", got, out)
			}
			if got := strings.Count(out, "limits:"); got != 2 {
				t.Errorf("expected a limits block on leader and worker (2), got %d\n---\n%s", got, out)
			}
		})
	}
}

// TestShardManifestPinsBackend: the scaffold's entire contract — workers
// addressed by host IP, no pod network, host GPU visible — is vk-ollama's
// native-process behaviour. Without this term the scheduler is free to
// place a shard on vk-podman, where llama-server would run inside
// podman's Linux VM and (on macOS/Windows) see no GPU at all. Observed
// live: leader and worker both landed on a vk-podman node.
func TestShardManifestPinsBackend(t *testing.T) {
	out := renderShard(t, baseShardInput())
	if got := strings.Count(out, "key: outpost.dhnt.io/backend"); got != 2 {
		t.Errorf("expected backend affinity on leader and worker (2), got %d\n---\n%s", got, out)
	}
	if !strings.Contains(out, "- vk-ollama") {
		t.Errorf("backend value missing\n---\n%s", out)
	}

	// Opt-out leaves it unconstrained.
	in := baseShardInput()
	in.backend = ""
	if out := renderShard(t, in); strings.Contains(out, "outpost.dhnt.io/backend") {
		t.Errorf("empty --backend must drop the term\n---\n%s", out)
	}
}
