// Package clusterconform is the single source of truth for the mixed-runtime
// coexistence contract on a peer-hosted DKS control plane — the set of node
// runtimes that may register against ONE apiserver at once (a real k3s agent
// plus one or more virtual-kubelet backends), the node-identity labels and
// taints each one carries, and the node-level API surface each one actually
// serves.
//
// WHY THIS EXISTS. That contract was previously implicit, spread across four
// packages that each re-derived a slice of it:
//
//   - vknode builds the virtual Node (its provider taint, its capacity) and
//     implements only the Pod lifecycle subset of the provider interface;
//   - the k3s-agent entrypoint stamps the real-agent node-identity labels;
//   - nodegc decides which nodes may be reaped when stale;
//   - nodecap / nodeaddr decide which nodes are probed / address-reconciled.
//
// A workload author, the scheduler, and an operator all reason about "what can
// I do on this node", and the answer differs by runtime:
//
//   - a REAL k3s-agent node serves the full kubelet API — `kubectl exec`,
//     `kubectl logs -f`, `kubectl port-forward`, `kubectl top`, attach — via a
//     kubelet streaming server tunnelled over Remotedialer.
//   - a VIRTUAL-KUBELET node (vk-podman / vk-native / vk-ollama) serves ONLY
//     the Pod lifecycle subset (create / update / delete / status / list). It
//     runs no kubelet streaming server, so the streaming verbs are
//     structurally unavailable; job results come back through the opt-in
//     termination-message log tail and the transient app router, not through
//     `kubectl logs`.
//
// The companion matrix_test.go is the deterministic conformance GATE: it holds
// the live wiring (vknode.BuildNode, the vknode.Provider method set, the
// nodegc / nodecap / nodeaddr exclusion predicates, and the k3s entrypoint's
// --node-label flags) to this matrix, so a change that lets a virtual node
// claim a real-agent API — or that drops a runtime's identity label — fails
// the build instead of shipping. The gate needs no podman, no apiserver, and
// no GPU: it asserts the CONTRACT, never live hardware behaviour.
package clusterconform

import "sort"

// Node-identity label keys. Identical across every placement (cloud-hosted and
// peer-hosted) so a workload's selectors and a controller's exclusions read the
// same node whichever plane runs the cluster.
const (
	RuntimeLabelKey  = "outpost.dhnt.io/runtime"
	BackendLabelKey  = "outpost.dhnt.io/backend"
	HostLabelKey     = "outpost.dhnt.io/host"
	ProviderTaintKey = "virtual-kubelet.io/provider"
)

// RuntimeClass partitions the node population into the two families that
// coexist on one control plane.
type RuntimeClass string

const (
	// ClassAgent is a real k3s agent: a kubelet + containerd in a container,
	// serving the full Kubernetes node API.
	ClassAgent RuntimeClass = "agent"
	// ClassVirtual is a virtual-kubelet backend: a provider that realizes Pods
	// on a substrate (podman / native process / ollama) and serves only the
	// lifecycle subset.
	ClassVirtual RuntimeClass = "virtual"
)

// Capability names one node-level API surface a runtime may or may not serve.
type Capability string

const (
	// CapPodLifecycle is the create / update / delete / status / list surface
	// every runtime serves — it is what makes a node schedulable at all.
	CapPodLifecycle Capability = "pod-lifecycle"

	// The kubelet STREAMING surface. Served only by real-agent nodes; a
	// virtual-kubelet node runs no kubelet server to answer these.
	CapKubeletLogStream   Capability = "kubelet-log-stream"   // kubectl logs [-f]
	CapKubeletExec        Capability = "kubelet-exec"         // kubectl exec
	CapKubeletAttach      Capability = "kubelet-attach"       // kubectl attach
	CapKubeletPortForward Capability = "kubelet-port-forward" // kubectl port-forward
	CapKubeletStats       Capability = "kubelet-stats"        // kubectl top / metrics

	// Virtual-node RESULT semantics — the channels a virtual node offers
	// INSTEAD of the streaming API. A real agent has no need of them.
	CapTerminationLogTail Capability = "termination-log-tail" // opt-in log tail into ContainerStateTerminated.Message
	CapTransientAppRoute  Capability = "transient-app-route"  // hostPort published into the outpost app router
)

// KubeletStreamingCapabilities is the set that requires a real kubelet server.
// A virtual-kubelet node must serve NONE of these — that is the load-bearing
// half of the "distinguishes virtual-node APIs from real-agent Kubernetes
// APIs" contract.
var KubeletStreamingCapabilities = []Capability{
	CapKubeletLogStream,
	CapKubeletExec,
	CapKubeletAttach,
	CapKubeletPortForward,
	CapKubeletStats,
}

// RuntimeProfile is the full coexistence contract for one runtime, keyed by its
// backend-label value (the node-identity fact, e.g. "k3s", "vk-podman").
type RuntimeProfile struct {
	// Backend is outpost.dhnt.io/backend on the node (the map key).
	Backend string
	// Class is the runtime family.
	Class RuntimeClass
	// RuntimeLabelValue is outpost.dhnt.io/runtime on the node.
	RuntimeLabelValue string
	// NodeNameSuffix is appended to the host's base node name to form this
	// runtime's node name. Empty for the agent (k3s appends its own
	// --with-node-id suffix instead).
	NodeNameSuffix string
	// ProviderTaint is true when the node carries the virtual-kubelet provider
	// NoSchedule taint (workloads opt in via toleration).
	ProviderTaint bool
	// SubjectToNodeGC is true when nodegc may reap this runtime's node once
	// stale. Only real k3s agents qualify — a virtual node has no kubelet lease
	// worth reaping and is re-minted trivially.
	SubjectToNodeGC bool
	// SubjectToRuntimeProbe is true when nodecap's sandbox probe evaluates this
	// runtime. Virtual nodes have no container runtime to probe.
	SubjectToRuntimeProbe bool
	// SubjectToAddressReconcile is true when nodeaddr patches this runtime's
	// kubelet address. Virtual nodes run no kubelet to reach.
	SubjectToAddressReconcile bool

	served map[Capability]bool
}

// Serves reports whether the runtime serves capability c.
func (p RuntimeProfile) Serves(c Capability) bool { return p.served[c] }

// ServesKubeletStreaming reports whether the runtime serves ANY kubelet
// streaming API. True only for real-agent nodes.
func (p RuntimeProfile) ServesKubeletStreaming() bool {
	for _, c := range KubeletStreamingCapabilities {
		if p.served[c] {
			return true
		}
	}
	return false
}

// agentServed / virtualServed are the two capability sets. Declared once so a
// new virtual backend cannot silently drift from the others.
var (
	agentServed = capSet(
		CapPodLifecycle,
		CapKubeletLogStream, CapKubeletExec, CapKubeletAttach,
		CapKubeletPortForward, CapKubeletStats,
	)
	virtualServed = capSet(
		CapPodLifecycle,
		CapTerminationLogTail,
		CapTransientAppRoute,
	)
)

// Profiles is the coexistence matrix, keyed by backend-label value.
//
// The virtual backends deliberately share one served-capability set and one
// class: they differ in SUBSTRATE (how a Pod is realized), never in the
// node-level API surface they expose. Keeping them identical here is what makes
// "add a fourth virtual backend" a one-line, gate-checked change.
var Profiles = map[string]RuntimeProfile{
	BackendK3s: {
		Backend:                   BackendK3s,
		Class:                     ClassAgent,
		RuntimeLabelValue:         string(ClassAgent),
		NodeNameSuffix:            "",
		ProviderTaint:             false,
		SubjectToNodeGC:           true,
		SubjectToRuntimeProbe:     true,
		SubjectToAddressReconcile: true,
		served:                    agentServed,
	},
	BackendVKPodman: virtualProfile(BackendVKPodman),
	BackendVKNative: virtualProfile(BackendVKNative),
	BackendVKOllama: virtualProfile(BackendVKOllama),
}

// Backend-label values. The vk-* values equal the conf runtime-selection
// strings (conf.ClusterRuntimeVK*); the gate asserts that equality so the
// operator-typed selection and the stamped node label can never diverge.
const (
	BackendK3s      = "k3s"
	BackendVKPodman = "vk-podman"
	BackendVKNative = "vk-native"
	BackendVKOllama = "vk-ollama"
)

func virtualProfile(backend string) RuntimeProfile {
	return RuntimeProfile{
		Backend:                   backend,
		Class:                     ClassVirtual,
		RuntimeLabelValue:         string(ClassVirtual),
		NodeNameSuffix:            "-" + backend,
		ProviderTaint:             true,
		SubjectToNodeGC:           false,
		SubjectToRuntimeProbe:     false,
		SubjectToAddressReconcile: false,
		served:                    virtualServed,
	}
}

func capSet(caps ...Capability) map[Capability]bool {
	m := make(map[Capability]bool, len(caps))
	for _, c := range caps {
		m[c] = true
	}
	return m
}

// Profile returns the profile for a backend-label value.
func Profile(backend string) (RuntimeProfile, bool) {
	p, ok := Profiles[backend]
	return p, ok
}

// AgentBackend is the backend-label value of the one real-agent runtime.
const AgentBackend = BackendK3s

// VirtualBackends returns the virtual backend-label values, sorted.
func VirtualBackends() []string {
	out := make([]string, 0, len(Profiles))
	for backend, p := range Profiles {
		if p.Class == ClassVirtual {
			out = append(out, backend)
		}
	}
	sort.Strings(out)
	return out
}

// NodeName composes the node name a runtime registers under, given the host's
// base node name. For the agent this is the base itself (k3s adds its own
// node-id suffix at registration); for a virtual backend it is base+suffix.
func NodeName(base, backend string) string {
	p, ok := Profiles[backend]
	if !ok {
		return base
	}
	return base + p.NodeNameSuffix
}
