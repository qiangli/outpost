package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/template"

	"github.com/qiangli/outpost/internal/agent/clusterllm"
	"github.com/spf13/cobra"
)

// clusterShardInitCmd scaffolds the manifest for a *sharded* llama.cpp
// inference topology: one coordinator (leader) running
// `llama-server --rpc <worker host IPs>` plus N RPC workers each running
// `rpc-server`. This is the cross-machine "model bigger than any one
// box's VRAM" case — the leader pipelines tensor work out to the workers
// over the llama.cpp RPC protocol.
//
// Two shapes, selected by --topology:
//
//   - lws (default): a LeaderWorkerSet, the natural fit for one logical
//     group of 1 leader + (size-1) workers that scale and roll together.
//     Requires the LeaderWorkerSet controller (sigs.k8s.io/lws) installed
//     in the cluster.
//   - deployment: a plain Deployment (the leader) + a headless Service +
//     a worker Deployment, for clusters without the LWS CRD. Functionally
//     equivalent; the headless Service is for `podman ps`/DNS legibility
//     only — the leader still addresses workers by host IP (see below).
//
// KNOWN TRAP — workers are addressed by host IP, not pod DNS. The
// vk-ollama backend lands these pods as *native* host processes (no pod
// network, no CNI), so the usual pod-IP / headless-DNS endpoints don't
// resolve to the rpc-server listeners. The leader's `--rpc` argument is
// therefore baked from the concrete --worker-ips the operator passes;
// the headless Service in deployment-mode is cosmetic. This keeps the
// whole thing renderable + testable fully offline.
//
// Placement: nodeAffinity pins every replica to whichever of lan-group /
// tier / gpu-kind the operator supplies — RPC sharding is latency-
// sensitive, so we never want the scheduler spreading a shard group
// across a WAN boundary. Each term is OPTIONAL and omitted when unset:
// these are requiredDuringScheduling terms, so naming a label no node
// carries yields a manifest that matches nothing at all. Each container requests dhnt.io/vram — the
// vendor-neutral GPU-memory resource vknode advertises in node capacity
// (see internal/agent/vknode/node.go) — so the scheduler only lands
// shards on boxes with the memory to hold their slice. Vendor is a
// SEPARATE axis: --gpu-kind adds an affinity on
// outpost.dhnt.io/gpu-kind, because a llama.cpp build is CUDA or Metal
// or ROCm and a shard group must not mix them.
func clusterShardInitCmd() *cobra.Command {
	var (
		name       string
		image      string
		model      string
		workerIPs  string
		rpcPort    int
		port       int
		lanGroup   string
		tier       string
		gpuKind    string
		backend    string
		leaderVRAM string
		workerVRAM string
		topology   string
		outPath    string
	)
	cmd := &cobra.Command{
		Use:   "shard-init",
		Short: "Scaffold a sharded llama.cpp (leader llama-server + rpc-server workers) manifest",
		Long: `Emit a kubectl-ready manifest for a cross-machine llama.cpp shard:
a coordinator running 'llama-server --rpc <worker IPs>' plus one
rpc-server worker per --worker-ips entry. nodeAffinity pins the group
to whichever of --lan-group / --tier / --gpu-kind you supply, and every
container requests dhnt.io/vram. Workers are addressed by host IP because they run
as native host processes with no pod network.

Example:

  outpost cluster shard-init --name llama70b \
    --image ghcr.io/ggml-org/llama.cpp:full \
    --model /models/llama-70b-q4.gguf \
    --worker-ips 192.168.1.21,192.168.1.22 \
    --gpu-kind nvidia --tier lan \
    --leader-vram 24Gi --worker-vram 24Gi | kubectl apply -f -`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := buildShardVars(shardInput{
				name:       name,
				image:      image,
				model:      model,
				workerIPs:  workerIPs,
				rpcPort:    rpcPort,
				port:       port,
				lanGroup:   lanGroup,
				tier:       tier,
				gpuKind:    gpuKind,
				backend:    backend,
				leaderVRAM: leaderVRAM,
				workerVRAM: workerVRAM,
				topology:   topology,
			})
			if err != nil {
				return err
			}
			var out io.Writer = os.Stdout
			if outPath != "" {
				f, err := os.Create(outPath)
				if err != nil {
					return err
				}
				defer f.Close()
				out = f
			}
			if err := renderShardManifest(out, data); err != nil {
				return err
			}
			if outPath != "" {
				fmt.Fprintf(os.Stderr, "wrote %s — apply with: kubectl apply -f %s\n", outPath, outPath)
			}
			fmt.Fprint(os.Stderr, shardAdvertiseHint(data))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Shard group name (leader + workers + Service prefix) [required]")
	cmd.Flags().StringVar(&image, "image", "ghcr.io/ggml-org/llama.cpp:full", "llama.cpp container image (must ship llama-server + rpc-server)")
	cmd.Flags().StringVar(&model, "model", "", "Model path inside the container (e.g. /models/llama-70b.gguf) [required]")
	cmd.Flags().StringVar(&workerIPs, "worker-ips", "", "Comma-separated rpc-server host IPs the leader shards to [required]")
	cmd.Flags().IntVar(&rpcPort, "rpc-port", 50052, "Port each rpc-server listens on")
	cmd.Flags().IntVar(&port, "port", 8080, "Port the leader llama-server serves the OpenAI/Ollama API on")
	cmd.Flags().StringVar(&lanGroup, "lan-group", "", "nodeAffinity: outpost.dhnt.io/lan-group value; empty = no LAN constraint (the label is only stamped on nodes with measured locality)")
	cmd.Flags().StringVar(&tier, "tier", "", "nodeAffinity: outpost.dhnt.io/tier value (tp|lan|wan|remote); empty = no tier constraint")
	cmd.Flags().StringVar(&backend, "backend", "vk-ollama", "nodeAffinity: outpost.dhnt.io/backend value — the vk backend that realizes these pods (vk-ollama runs them as native host processes, which is what a shard needs); empty = any backend")
	cmd.Flags().StringVar(&gpuKind, "gpu-kind", "", "nodeAffinity: outpost.dhnt.io/gpu-kind value (nvidia|apple-silicon|amd|intel); empty = any vendor. Set it — a llama.cpp build is CUDA or Metal, never both")
	cmd.Flags().StringVar(&leaderVRAM, "leader-vram", "8Gi", "dhnt.io/vram request for the leader (vendor-neutral GPU memory)")
	cmd.Flags().StringVar(&workerVRAM, "worker-vram", "8Gi", "dhnt.io/vram request per worker (vendor-neutral GPU memory)")
	cmd.Flags().StringVar(&topology, "topology", "lws", "Output shape: lws (LeaderWorkerSet) or deployment (Deployment + headless Service)")
	cmd.Flags().StringVar(&outPath, "output", "", "Write to PATH instead of stdout")
	return cmd
}

type shardInput struct {
	name       string
	image      string
	model      string
	workerIPs  string
	rpcPort    int
	port       int
	lanGroup   string
	tier       string
	gpuKind    string
	backend    string
	leaderVRAM string
	workerVRAM string
	topology   string
}

// shardVars is the rendering model the templates consume. Lists are
// pre-split so the templates stay logic-light.
type shardVars struct {
	Name       string
	Image      string
	Model      string
	WorkerIPs  []string // concrete rpc-server host IPs
	RPCPort    int
	Port       int
	LANGroup   string
	Tier       string
	GPUKind    string
	Backend    string
	LeaderVRAM string
	WorkerVRAM string
	UseLWS     bool
	// RPCEndpoints is the comma-joined ip:port list fed to the leader's
	// --rpc flag. Precomputed because workers are addressed by host IP,
	// not pod DNS (see command doc).
	RPCEndpoints string
	// WorkerCount is len(WorkerIPs); for LWS this drives size = N+1.
	WorkerCount int
}

func buildShardVars(in shardInput) (shardVars, error) {
	if strings.TrimSpace(in.name) == "" {
		return shardVars{}, errors.New("--name required")
	}
	if strings.TrimSpace(in.image) == "" {
		return shardVars{}, errors.New("--image required")
	}
	if strings.TrimSpace(in.model) == "" {
		return shardVars{}, errors.New("--model required")
	}
	if in.rpcPort <= 0 || in.rpcPort > 65535 {
		return shardVars{}, errors.New("--rpc-port must be 1..65535")
	}
	if in.port <= 0 || in.port > 65535 {
		return shardVars{}, errors.New("--port must be 1..65535")
	}
	tier := strings.TrimSpace(in.tier)
	// The --backend flag defaults to vk-ollama, the backend this whole
	// scaffold is written around: it realizes a Pod as a NATIVE host
	// process, which is why workers are addressed by host IP and why the
	// leader can see the host GPU at all. Landing these pods on
	// vk-podman instead would run llama-server inside podman's Linux VM
	// — different semantics, and on macOS/Windows no host GPU at all.
	// Pass --backend "" to opt out of the constraint.
	backend := strings.TrimSpace(in.backend)

	var ips []string
	for raw := range strings.SplitSeq(in.workerIPs, ",") {
		if ip := strings.TrimSpace(raw); ip != "" {
			ips = append(ips, ip)
		}
	}
	if len(ips) == 0 {
		return shardVars{}, errors.New("--worker-ips required (at least one rpc-server host IP)")
	}

	endpoints := make([]string, len(ips))
	for i, ip := range ips {
		endpoints[i] = fmt.Sprintf("%s:%d", ip, in.rpcPort)
	}

	topo := strings.ToLower(strings.TrimSpace(in.topology))
	switch topo {
	case "", "lws":
		topo = "lws"
	case "deployment", "deploy":
		topo = "deployment"
	default:
		return shardVars{}, fmt.Errorf("--topology must be lws or deployment, got %q", in.topology)
	}

	return shardVars{
		Name:         in.name,
		Image:        in.image,
		Model:        in.model,
		WorkerIPs:    ips,
		RPCPort:      in.rpcPort,
		Port:         in.port,
		LANGroup:     in.lanGroup,
		Tier:         tier,
		GPUKind:      in.gpuKind,
		Backend:      backend,
		LeaderVRAM:   in.leaderVRAM,
		WorkerVRAM:   in.workerVRAM,
		UseLWS:       topo == "lws",
		RPCEndpoints: strings.Join(endpoints, ","),
		WorkerCount:  len(ips),
	}, nil
}

// shardAdvertiseHint returns the formation-time advisory printed after the
// manifest: how to advertise the freshly-scaffolded shard to cloudbox
// through the EXISTING cluster_llm_endpoint seam. The leader llama-server
// serves the OpenAI/Ollama API on its host port; on the node it lands on,
// pointing cluster_llm_endpoint at the loopback serving URL makes that
// outpost's clusterllm detector recognize the llama.cpp shard leader and
// publish it (backend=llamacpp) on the registry push, so cloudbox's tier-0
// router can route to this home. No new config surface — it reuses the
// GPUStack seam already wired through `outpost config set`.
func shardAdvertiseHint(data shardVars) string {
	endpoint := clusterllm.ShardEndpoint(data.Port)
	var b strings.Builder
	fmt.Fprintf(&b, "\nshard %q scaffolded: leader serves the OpenAI/Ollama API on :%d across %d nodes (1 leader + %d workers).\n",
		data.Name, data.Port, data.WorkerCount+1, data.WorkerCount)
	b.WriteString("To advertise this cluster to cloudbox through the existing cluster_llm seam, on the node running the leader run:\n")
	fmt.Fprintf(&b, "  outpost config set --cluster-llm-endpoint %s\n", endpoint)
	fmt.Fprintf(&b, "The outpost there detects the llama.cpp shard leader and advertises it (backend=%s).\n", clusterllm.BackendLlamaCPP)
	return b.String()
}

func renderShardManifest(out io.Writer, data shardVars) error {
	tmpl := template.Must(template.New("shard").
		Funcs(template.FuncMap{
			"add1":         func(n int) int { return n + 1 },
			"nodeAffinity": nodeAffinityBlock,
			"vkToleration": vkTolerationBlock,
		}).
		Parse(shardManifestTemplate))
	return tmpl.Execute(out, data)
}

// vkTolerationBlock renders the virtual-kubelet provider toleration,
// indented by `pad` spaces. Returned without a trailing newline.
//
// Every vknode registers its Node with the taint
// virtual-kubelet.io/provider=outpost:NoSchedule, so a pod WITHOUT this
// toleration is refused by exactly the nodes a shard is meant to run
// on — the vk-ollama nodes that realize these pods as native host
// processes. `outpost cluster init` has always emitted it; shard-init
// did not, which meant a scaffolded shard could never be placed even
// once its resource request was satisfiable.
func vkTolerationBlock(pad int) string {
	p := strings.Repeat(" ", pad)
	var b strings.Builder
	fmt.Fprintf(&b, "%stolerations:\n", p)
	fmt.Fprintf(&b, "%s- key: virtual-kubelet.io/provider\n", p)
	fmt.Fprintf(&b, "%s  operator: Exists", p)
	return b.String()
}

// nodeAffinityBlock renders the placement nodeAffinity YAML indented by
// `pad` spaces, so the same placement contract drops cleanly into both
// the LWS pod specs (deeper nesting) and the Deployment pod specs
// (shallower). Returned without a trailing newline; call sites prefix it
// with `{{- nodeAffinity N .LANGroup .Tier .GPUKind .Backend}}` on its own line.
//
// EVERY term is omitted when its value is empty, and that is load-bearing
// rather than mere tidiness: these are requiredDuringScheduling terms, so
// a term naming a label no node carries makes the whole manifest match
// zero nodes. lan-group in particular is stamped only when cloudbox has
// issued measured locality data (see vknode.NodeLocalityLabels), which on
// a young fleet is no node at all — emitting it unconditionally is how a
// scaffold ends up rendering a manifest that can never be placed.
func nodeAffinityBlock(pad int, lanGroup, tier, gpuKind, backend string) string {
	type term struct{ key, value string }
	terms := []term{
		{"outpost.dhnt.io/lan-group", lanGroup},
		{"outpost.dhnt.io/tier", tier},
		{"outpost.dhnt.io/gpu-kind", gpuKind},
		{"outpost.dhnt.io/backend", backend},
	}
	present := make([]term, 0, len(terms))
	for _, t := range terms {
		if strings.TrimSpace(t.value) != "" {
			present = append(present, t)
		}
	}
	if len(present) == 0 {
		return ""
	}
	p := strings.Repeat(" ", pad)
	var b strings.Builder
	fmt.Fprintf(&b, "%saffinity:\n", p)
	fmt.Fprintf(&b, "%s  nodeAffinity:\n", p)
	fmt.Fprintf(&b, "%s    requiredDuringSchedulingIgnoredDuringExecution:\n", p)
	fmt.Fprintf(&b, "%s      nodeSelectorTerms:\n", p)
	fmt.Fprintf(&b, "%s      - matchExpressions:\n", p)
	for i, t := range present {
		fmt.Fprintf(&b, "%s        - key: %s\n", p, t.key)
		fmt.Fprintf(&b, "%s          operator: In\n", p)
		fmt.Fprintf(&b, "%s          values:\n", p)
		if i == len(present)-1 {
			fmt.Fprintf(&b, "%s          - %s", p, t.value)
		} else {
			fmt.Fprintf(&b, "%s          - %s\n", p, t.value)
		}
	}
	return b.String()
}

// shardManifestTemplate renders either a LeaderWorkerSet or a
// Deployment-pair. YAML is whitespace-sensitive — validate changes with
// `kubectl apply --dry-run=client -f -`.
//
// The nodeAffinity block is rendered by the nodeAffinity FuncMap helper
// so leader and worker pods stamp the identical placement contract at the
// indentation each context requires.
const shardManifestTemplate = `{{- if .UseLWS -}}
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: {{.Name}}
  labels:
    app: {{.Name}}
spec:
  replicas: 1
  leaderWorkerTemplate:
    # 1 leader + {{.WorkerCount}} workers.
    size: {{add1 .WorkerCount}}
    leaderTemplate:
      metadata:
        labels:
          app: {{.Name}}
          role: leader
      spec:
{{vkToleration 8}}
{{nodeAffinity 8 .LANGroup .Tier .GPUKind .Backend}}
        containers:
        - name: leader
          image: {{.Image}}
          imagePullPolicy: IfNotPresent
          # Workers are addressed by host IP — these pods run as native
          # host processes with no pod network, so pod DNS does not
          # resolve to the rpc-server listeners.
          command: ["llama-server"]
          args:
          - "--model"
          - "{{.Model}}"
          - "--host"
          - "0.0.0.0"
          - "--port"
          - "{{.Port}}"
          - "--rpc"
          - "{{.RPCEndpoints}}"
          ports:
          - containerPort: {{.Port}}
          resources:
            # An extended resource is NON-OVERCOMMITTABLE: Kubernetes
            # rejects a pod that names one in requests without an equal
            # limit ("Limit must be set for non overcommitable
            # resources"). requests alone never even reached the
            # scheduler — the API server refused the object.
            requests:
              dhnt.io/vram: {{.LeaderVRAM}}
            limits:
              dhnt.io/vram: {{.LeaderVRAM}}
    workerTemplate:
      metadata:
        labels:
          app: {{.Name}}
          role: worker
      spec:
        hostNetwork: true
{{vkToleration 8}}
{{nodeAffinity 8 .LANGroup .Tier .GPUKind .Backend}}
        containers:
        - name: worker
          image: {{.Image}}
          imagePullPolicy: IfNotPresent
          command: ["rpc-server"]
          args:
          - "--host"
          - "0.0.0.0"
          - "--port"
          - "{{.RPCPort}}"
          ports:
          - containerPort: {{.RPCPort}}
            hostPort: {{.RPCPort}}
          resources:
            requests:
              dhnt.io/vram: {{.WorkerVRAM}}
            limits:
              dhnt.io/vram: {{.WorkerVRAM}}
{{- else -}}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{.Name}}-leader
  labels:
    app: {{.Name}}
    role: leader
spec:
  replicas: 1
  selector:
    matchLabels:
      app: {{.Name}}
      role: leader
  template:
    metadata:
      labels:
        app: {{.Name}}
        role: leader
    spec:
{{vkToleration 6}}
{{nodeAffinity 6 .LANGroup .Tier .GPUKind .Backend}}
      containers:
      - name: leader
        image: {{.Image}}
        imagePullPolicy: IfNotPresent
        # Workers are addressed by host IP — native host processes, no
        # pod network — so the headless Service below is for legibility
        # only and the --rpc list is baked from --worker-ips.
        command: ["llama-server"]
        args:
        - "--model"
        - "{{.Model}}"
        - "--host"
        - "0.0.0.0"
        - "--port"
        - "{{.Port}}"
        - "--rpc"
        - "{{.RPCEndpoints}}"
        ports:
        - containerPort: {{.Port}}
        resources:
          # Extended resources need an equal limit — see the LWS branch.
          requests:
            dhnt.io/vram: {{.LeaderVRAM}}
          limits:
            dhnt.io/vram: {{.LeaderVRAM}}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{.Name}}-worker
  labels:
    app: {{.Name}}
    role: worker
spec:
  replicas: {{.WorkerCount}}
  selector:
    matchLabels:
      app: {{.Name}}
      role: worker
  template:
    metadata:
      labels:
        app: {{.Name}}
        role: worker
    spec:
      hostNetwork: true
{{vkToleration 6}}
{{nodeAffinity 6 .LANGroup .Tier .GPUKind .Backend}}
      containers:
      - name: worker
        image: {{.Image}}
        imagePullPolicy: IfNotPresent
        command: ["rpc-server"]
        args:
        - "--host"
        - "0.0.0.0"
        - "--port"
        - "{{.RPCPort}}"
        ports:
        - containerPort: {{.RPCPort}}
          hostPort: {{.RPCPort}}
        resources:
          requests:
            dhnt.io/vram: {{.WorkerVRAM}}
          limits:
            dhnt.io/vram: {{.WorkerVRAM}}
---
apiVersion: v1
kind: Service
metadata:
  name: {{.Name}}
  labels:
    app: {{.Name}}
spec:
  # Headless: leader serves the API on its host; this Service exists for
  # discovery/legibility. Workers are reached by host IP, not via DNS.
  clusterIP: None
  selector:
    app: {{.Name}}
    role: leader
  ports:
  - port: {{.Port}}
    targetPort: {{.Port}}
{{- end -}}
`
