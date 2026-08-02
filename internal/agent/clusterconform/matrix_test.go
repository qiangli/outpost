package clusterconform

// Deterministic mixed-runtime conformance GATE.
//
// These tests hold the live coexistence wiring to the matrix in matrix.go —
// no podman, no apiserver, no GPU. They assert the CONTRACT that lets a real
// k3s agent and one or more virtual-kubelet backends register against ONE
// control plane at once, and they draw the line the task names explicitly:
// which node-level APIs a VIRTUAL node serves versus which require a REAL
// agent's kubelet. A change that erases that line fails here.

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/nodeaddr"
	"github.com/qiangli/outpost/internal/agent/nodecap"
	"github.com/qiangli/outpost/internal/agent/nodegc"
	"github.com/qiangli/outpost/internal/agent/vknode"

	"github.com/virtual-kubelet/virtual-kubelet/node"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// --- 1. The matrix is internally self-consistent -------------------------

func TestMatrixInternalConsistency(t *testing.T) {
	var agents, virtuals int
	for backend, p := range Profiles {
		if p.Backend != backend {
			t.Errorf("%s: Backend field %q disagrees with map key", backend, p.Backend)
		}
		if !p.Serves(CapPodLifecycle) {
			t.Errorf("%s: every runtime must serve Pod lifecycle", backend)
		}
		switch p.Class {
		case ClassAgent:
			agents++
			if p.RuntimeLabelValue != string(ClassAgent) {
				t.Errorf("%s: agent runtime label = %q", backend, p.RuntimeLabelValue)
			}
			if !p.ServesKubeletStreaming() {
				t.Errorf("%s: a real agent must serve the kubelet streaming API", backend)
			}
			if p.ProviderTaint {
				t.Errorf("%s: a real agent must not carry the vk provider taint", backend)
			}
			if !p.SubjectToNodeGC || !p.SubjectToRuntimeProbe || !p.SubjectToAddressReconcile {
				t.Errorf("%s: a real agent is subject to GC, runtime-probe, and address reconcile", backend)
			}
		case ClassVirtual:
			virtuals++
			if p.RuntimeLabelValue != string(ClassVirtual) {
				t.Errorf("%s: virtual runtime label = %q", backend, p.RuntimeLabelValue)
			}
			// The load-bearing line: NO streaming capability on a virtual node.
			for _, c := range KubeletStreamingCapabilities {
				if p.Serves(c) {
					t.Errorf("%s: virtual node must not serve kubelet API %q", backend, c)
				}
			}
			if p.ServesKubeletStreaming() {
				t.Errorf("%s: virtual node reports serving streaming API", backend)
			}
			// The result channels a virtual node offers instead.
			if !p.Serves(CapTerminationLogTail) || !p.Serves(CapTransientAppRoute) {
				t.Errorf("%s: virtual node must offer termination-log-tail + transient-app-route", backend)
			}
			if !p.ProviderTaint {
				t.Errorf("%s: virtual node must carry the vk provider taint", backend)
			}
			if p.SubjectToNodeGC || p.SubjectToRuntimeProbe || p.SubjectToAddressReconcile {
				t.Errorf("%s: virtual node must be excluded from GC, runtime-probe, and address reconcile", backend)
			}
			if p.NodeNameSuffix != "-"+backend {
				t.Errorf("%s: node-name suffix = %q, want -%s", backend, p.NodeNameSuffix, backend)
			}
		default:
			t.Errorf("%s: unknown class %q", backend, p.Class)
		}
	}
	if agents != 1 {
		t.Errorf("expected exactly one real-agent runtime, got %d", agents)
	}
	if virtuals < 1 {
		t.Errorf("expected at least one virtual runtime, got %d", virtuals)
	}
}

// --- 2. The label vocabulary is shared, not re-invented per package -------

func TestLabelVocabularyMatchesPeerPackages(t *testing.T) {
	// nodegc, nodecap, and nodeaddr each define the same node-identity
	// vocabulary locally. They MUST agree, or a node reaped by one controller
	// is invisible to another's exclusion check.
	if RuntimeLabelKey != nodegc.RuntimeLabel {
		t.Errorf("runtime label key: matrix %q vs nodegc %q", RuntimeLabelKey, nodegc.RuntimeLabel)
	}
	if BackendLabelKey != nodegc.BackendLabel {
		t.Errorf("backend label key: matrix %q vs nodegc %q", BackendLabelKey, nodegc.BackendLabel)
	}
	if HostLabelKey != nodegc.HostLabel {
		t.Errorf("host label key: matrix %q vs nodegc %q", HostLabelKey, nodegc.HostLabel)
	}
	if RuntimeLabelKey != nodecap.RuntimeLabel {
		t.Errorf("runtime label key: matrix %q vs nodecap %q", RuntimeLabelKey, nodecap.RuntimeLabel)
	}
	if RuntimeLabelKey != nodeaddr.RuntimeLabel {
		t.Errorf("runtime label key: matrix %q vs nodeaddr %q", RuntimeLabelKey, nodeaddr.RuntimeLabel)
	}

	agent := Profiles[AgentBackend]
	if agent.RuntimeLabelValue != nodegc.RuntimeAgent {
		t.Errorf("agent runtime value: matrix %q vs nodegc %q", agent.RuntimeLabelValue, nodegc.RuntimeAgent)
	}
	if agent.Backend != nodegc.BackendK3s {
		t.Errorf("agent backend value: matrix %q vs nodegc %q", agent.Backend, nodegc.BackendK3s)
	}
	if string(ClassVirtual) != nodecap.RuntimeVirtual {
		t.Errorf("virtual runtime value: matrix %q vs nodecap %q", ClassVirtual, nodecap.RuntimeVirtual)
	}
	if string(ClassVirtual) != nodeaddr.RuntimeVirtual {
		t.Errorf("virtual runtime value: matrix %q vs nodeaddr %q", ClassVirtual, nodeaddr.RuntimeVirtual)
	}
}

// --- 3. The virtual backends are exactly the conf runtime selections ------

func TestVirtualBackendsMatchConfRuntimes(t *testing.T) {
	confVirtual := map[string]bool{
		conf.ClusterRuntimeVKPodman: true,
		conf.ClusterRuntimeVKNative: true,
		conf.ClusterRuntimeVKOllama: true,
	}
	got := VirtualBackends()
	if len(got) != len(confVirtual) {
		t.Fatalf("virtual backends %v vs conf selections %v", got, confVirtual)
	}
	for _, backend := range got {
		if !confVirtual[backend] {
			t.Errorf("virtual backend %q is not a conf runtime selection", backend)
		}
		// A backend the config accepts must be a backend the matrix knows —
		// otherwise a joined runtime has no coexistence contract.
		if !conf.ValidVirtualRuntime(backend) {
			t.Errorf("matrix virtual backend %q rejected by conf.ValidVirtualRuntime", backend)
		}
	}
	// The agent selection maps to the agent profile.
	if conf.ClusterRuntimeAgent != string(ClassAgent) {
		t.Errorf("conf agent runtime %q != class %q", conf.ClusterRuntimeAgent, ClassAgent)
	}
}

// --- 4. vknode.Provider serves lifecycle, NEVER the streaming API ----------

// streamingProviderMethods are the entry points virtual-kubelet auto-wires
// into a kubelet streaming server. A provider that exposes any of them can
// answer `kubectl logs`/`exec`/`attach`/`port-forward`/`top`. *vknode.Provider
// must expose NONE — it implements only PodLifecycleHandler — which is the
// structural reason a virtual node cannot serve those verbs. The method-name
// check (not a typed-interface assertion) is deliberate: it needs no heavy
// streaming dependency in go.mod and flips regardless of the exact signature
// someone might add, forcing an explicit matrix update when a virtual node
// gains a real-agent API.
var streamingProviderMethods = map[string]Capability{
	"GetContainerLogs":   CapKubeletLogStream,
	"RunInContainer":     CapKubeletExec,
	"AttachToContainer":  CapKubeletAttach,
	"PortForward":        CapKubeletPortForward,
	"GetStatsSummary":    CapKubeletStats,
	"GetMetricsResource": CapKubeletStats,
}

func TestProviderServesLifecycleNotStreaming(t *testing.T) {
	// Compile-time proof it IS a lifecycle provider.
	var _ node.PodLifecycleHandler = (*vknode.Provider)(nil)

	pt := reflect.TypeOf((*vknode.Provider)(nil))
	// Lifecycle methods MUST be present.
	for _, m := range []string{"CreatePod", "UpdatePod", "DeletePod", "GetPod", "GetPodStatus", "GetPods"} {
		if _, ok := pt.MethodByName(m); !ok {
			t.Errorf("vknode.Provider missing lifecycle method %s", m)
		}
	}
	// Streaming methods MUST be absent.
	for m, cap := range streamingProviderMethods {
		if _, ok := pt.MethodByName(m); ok {
			t.Errorf("vknode.Provider exposes %s (%s) — virtual nodes must not serve the kubelet streaming API; update the matrix if this is intentional", m, cap)
		}
	}
}

// --- 5. A built virtual Node carries the taint + identity the matrix says --

func TestVirtualNodeBuildCarriesTaintAndLabels(t *testing.T) {
	const base = "laptop"
	for _, backend := range VirtualBackends() {
		p := Profiles[backend]
		// Reproduce the label set cmd/outpost/main.go stamps for a virtual
		// runtime: runtime=virtual, backend=<mode>, host=<host>.
		labels := map[string]string{
			RuntimeLabelKey: p.RuntimeLabelValue,
			BackendLabelKey: backend,
			HostLabelKey:    base,
		}
		nodeName := NodeName(base, backend)
		if nodeName != base+"-"+backend {
			t.Errorf("%s: NodeName = %q", backend, nodeName)
		}
		n := vknode.BuildNode(nodeName, labels)

		if got := n.Labels[RuntimeLabelKey]; got != string(ClassVirtual) {
			t.Errorf("%s: node runtime label = %q", backend, got)
		}
		if got := n.Labels[BackendLabelKey]; got != backend {
			t.Errorf("%s: node backend label = %q", backend, got)
		}
		if !hasProviderTaint(n) {
			t.Errorf("%s: built node missing %s:NoSchedule taint", backend, ProviderTaintKey)
		}
		// Distinct node name per backend so two virtual runtimes on ONE host
		// are two distinct Node objects, never a name collision.
		if NodeName(base, BackendVKPodman) == NodeName(base, BackendVKNative) {
			t.Error("virtual backends collide on node name")
		}
	}
}

func hasProviderTaint(n *corev1.Node) bool {
	for _, tt := range n.Spec.Taints {
		if tt.Key == ProviderTaintKey && tt.Effect == corev1.TaintEffectNoSchedule {
			return true
		}
	}
	return false
}

// --- 6. Stale-node GC reaps agents, never virtual nodes -------------------

func TestNodeGCReapsAgentExcludesVirtual(t *testing.T) {
	const host = "laptop"
	grace := nodegc.DefaultGrace
	now := time.Now()
	stale := metav1.NewTime(now.Add(-2 * grace))

	agentNode := identityNode(host+"-abc123", map[string]string{
		nodegc.RuntimeLabel: nodegc.RuntimeAgent,
		nodegc.BackendLabel: nodegc.BackendK3s,
		nodegc.HostLabel:    host,
	}, corev1.ConditionFalse, stale)
	if _, ok := nodegc.StaleSince(agentNode, grace, now); !ok {
		t.Error("a long-NotReady real agent node must be a GC candidate")
	}

	for _, backend := range VirtualBackends() {
		virt := identityNode(NodeName(host, backend), map[string]string{
			nodegc.RuntimeLabel: string(ClassVirtual),
			nodegc.BackendLabel: backend,
			nodegc.HostLabel:    host,
		}, corev1.ConditionFalse, stale)
		if _, ok := nodegc.StaleSince(virt, grace, now); ok {
			t.Errorf("%s: virtual node must never be a GC candidate", backend)
		}
	}
}

func identityNode(name string, labels map[string]string, ready corev1.ConditionStatus, since metav1.Time) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type:               corev1.NodeReady,
				Status:             ready,
				LastTransitionTime: since,
			}},
		},
	}
}

// --- 7. Runtime-capability probe skips virtual nodes ----------------------

func TestNodeCapExcludesVirtual(t *testing.T) {
	agent := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "laptop-abc",
		Labels: map[string]string{nodecap.RuntimeLabel: nodegc.RuntimeAgent},
	}}
	if !nodecap.DefaultInclude(agent) {
		t.Error("nodecap must probe real agent nodes")
	}
	if nodecap.IsVirtualNode(agent) {
		t.Error("agent node misclassified as virtual")
	}
	for _, backend := range VirtualBackends() {
		virt := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:   NodeName("laptop", backend),
			Labels: map[string]string{nodecap.RuntimeLabel: string(ClassVirtual)},
		}}
		if !nodecap.IsVirtualNode(virt) {
			t.Errorf("%s: not recognized as virtual", backend)
		}
		if nodecap.DefaultInclude(virt) {
			t.Errorf("%s: virtual node must be excluded from the runtime probe", backend)
		}
	}
}

// --- 8. Address reconciliation skips virtual nodes ------------------------

func TestNodeAddrExcludesVirtual(t *testing.T) {
	for _, backend := range VirtualBackends() {
		virt := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:   NodeName("laptop", backend),
			Labels: map[string]string{nodeaddr.RuntimeLabel: string(ClassVirtual)},
		}}
		if !nodeaddr.IsVirtualNode(virt) {
			t.Errorf("%s: nodeaddr does not recognize it as virtual", backend)
		}
	}

	// End-to-end on a fake apiserver: one agent, one virtual node. Only the
	// agent is address-patched; the virtual node runs no kubelet to reach.
	agent := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "laptop-agent"}}
	virt := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   NodeName("laptop", BackendVKPodman),
		Labels: map[string]string{nodeaddr.RuntimeLabel: string(ClassVirtual)},
	}}
	cs := fake.NewSimpleClientset(agent, virt)
	r := &nodeaddr.Reconciler{Client: cs, PortFor: nodeaddr.DerivedKubeletPort}
	if err := r.Once(context.Background()); err != nil {
		t.Fatalf("nodeaddr Once: %v", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() != "patch" {
			continue
		}
		if na, ok := a.(interface{ GetName() string }); ok && na.GetName() == virt.Name {
			t.Error("nodeaddr patched a virtual-kubelet node")
		}
	}
}

// --- 9. The k3s-agent entrypoint stamps the matrix's agent identity -------

// The agent node's identity is stamped by a SHELL script (--node-label flags),
// not Go code, so it can drift from the matrix silently. Read it and assert.
func TestAgentEntrypointStampsMatrixIdentity(t *testing.T) {
	const path = "../runtime/image/entrypoint.sh"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("entrypoint not readable (%v) — skipping drift check", err)
	}
	script := string(data)
	agent := Profiles[AgentBackend]
	want := []string{
		"outpost.dhnt.io/runtime=" + agent.RuntimeLabelValue,
		"outpost.dhnt.io/backend=" + agent.Backend,
		"outpost.dhnt.io/host=",
	}
	for _, w := range want {
		if !strings.Contains(script, w) {
			t.Errorf("entrypoint.sh does not stamp %q — drifted from the matrix", w)
		}
	}
	// It must NOT stamp the virtual runtime value onto a real agent.
	if strings.Contains(script, "outpost.dhnt.io/runtime="+string(ClassVirtual)) {
		t.Error("entrypoint.sh stamps runtime=virtual onto the k3s agent")
	}
}
