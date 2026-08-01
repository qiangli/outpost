package nodeaddr

import (
	"context"
	"fmt"
	"net/netip"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The property the whole package exists for: no two nodes may share an
// address. A shared one collapses k3s's routing trie and sends every
// kubelet dial to one arbitrary node.
func TestLoopbackForNode_UniqueAcrossManyNodes(t *testing.T) {
	taken := map[string]string{}
	seen := map[string]string{}
	for i := 0; i < 500; i++ {
		name := fmt.Sprintf("node-%d", i)
		addr := LoopbackForNode(name, taken)
		if addr == "" {
			t.Fatalf("%s: no address allocated", name)
		}
		if prev, dup := seen[addr]; dup {
			t.Fatalf("%s and %s both got %s — collapses the routing trie", prev, name, addr)
		}
		seen[addr] = name
	}
}

// 127.0.0.1 is the tunnel's own bind address and the apiserver's
// advertise address; 127.0.0.0 is a network address. Minting either
// would reintroduce the exact collision this avoids.
func TestLoopbackForNode_NeverMints127_0_0_x(t *testing.T) {
	taken := map[string]string{}
	for i := 0; i < 2000; i++ {
		addr := LoopbackForNode(fmt.Sprintf("n%d", i), taken)
		ip, err := netip.ParseAddr(addr)
		if err != nil {
			t.Fatalf("%q is not an address: %v", addr, err)
		}
		if !ip.IsLoopback() {
			t.Fatalf("%s is outside 127.0.0.0/8", addr)
		}
		b := ip.As4()
		if b[1] == 0 && b[2] == 0 {
			t.Fatalf("%s falls inside 127.0.0.0/24, which is reserved", addr)
		}
	}
}

// Assignment must be reproducible: an unstable mapping would re-patch
// every node on every pass and churn addresses under a live cluster.
func TestLoopbackForNode_StableForSameInput(t *testing.T) {
	run := func() []string {
		taken := map[string]string{}
		var out []string
		for _, n := range []string{"alpha", "beta", "gamma", "delta"} {
			out = append(out, LoopbackForNode(n, taken))
		}
		return out
	}
	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("assignment %d differs between runs: %s vs %s", i, a[i], b[i])
		}
	}
}

// Re-probing an already-assigned node returns its own address rather than
// stepping past it — otherwise a second pass would move every node.
func TestLoopbackForNode_IdempotentForSameOwner(t *testing.T) {
	taken := map[string]string{}
	first := LoopbackForNode("worker", taken)
	if again := LoopbackForNode("worker", taken); again != first {
		t.Fatalf("same node got %s then %s", first, again)
	}
}

func node(name string, extIP string, port int32) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if extIP != "" {
		n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: extIP}}
	}
	n.Status.DaemonEndpoints.KubeletEndpoint.Port = port
	return n
}

func TestReconcile_PatchesNodesAndSkipsCorrectOnes(t *testing.T) {
	// "fresh" has nothing set and must be patched. "settled" already
	// carries the address it would be assigned, plus the right port, so
	// it must be left alone.
	taken := map[string]string{}
	for _, n := range []string{"fresh", "settled"} { // sorted order
		_ = LoopbackForNode(n, taken)
	}
	var settledAddr string
	for a, owner := range taken {
		if owner == "settled" {
			settledAddr = a
		}
	}

	cs := fake.NewSimpleClientset(
		node("fresh", "", 0),
		node("settled", settledAddr, 10250),
	)
	// Patches are read off the fake clientset's action log, so they still
	// apply normally rather than being intercepted by a reactor.
	patched := map[string]bool{}
	r := &Reconciler{
		Client:  cs,
		PortFor: func(string) (int, bool) { return 10250, true },
	}
	if err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "patch" {
			if pa, ok := a.(interface{ GetName() string }); ok {
				patched[pa.GetName()] = true
			}
		}
	}
	if !patched["fresh"] {
		t.Error("fresh node was not patched")
	}
	if patched["settled"] {
		t.Error("already-correct node was patched again — churns the API every pass")
	}
}

// A node whose tunnel is not up has no reachable kubelet port. Patching a
// bogus one would publish an address that dials nothing.
func TestReconcile_SkipsNodeWithNoPort(t *testing.T) {
	cs := fake.NewSimpleClientset(node("pending", "", 0))
	r := &Reconciler{
		Client:  cs,
		PortFor: func(string) (int, bool) { return 0, false },
	}
	if err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "patch" {
			t.Fatal("patched a node with no reachable kubelet port")
		}
	}
}

// Both ends derive the port independently — the same name must always
// give the same number, or the control plane patches a port the node
// never bound.
func TestKubeletPortForNode_Deterministic(t *testing.T) {
	for _, n := range []string{"alpha", "gpu-box", "laptop-vk-ollama"} {
		if a, b := KubeletPortForNode(n), KubeletPortForNode(n); a != b {
			t.Fatalf("%s: %d then %d", n, a, b)
		}
	}
}

// The range dodges three things that would collide in practice:
// privileged ports, Kubernetes' NodePort range, and Linux's ephemeral
// range that outbound sockets draw from.
func TestKubeletPortForNode_AvoidsReservedRanges(t *testing.T) {
	for i := 0; i < 5000; i++ {
		p := KubeletPortForNode(fmt.Sprintf("node-%d", i))
		switch {
		case p < 1024:
			t.Fatalf("%d is privileged", p)
		case p >= 30000 && p <= 32767:
			t.Fatalf("%d collides with the NodePort range", p)
		case p >= 32768:
			t.Fatalf("%d collides with the ephemeral range", p)
		}
	}
}

func TestDerivedKubeletPort(t *testing.T) {
	p, ok := DerivedKubeletPort("gpu-box")
	if !ok || p != KubeletPortForNode("gpu-box") {
		t.Fatalf("got %d/%v, want %d/true", p, ok, KubeletPortForNode("gpu-box"))
	}
	if _, ok := DerivedKubeletPort(""); ok {
		t.Error("an empty node name must not yield a port")
	}
}
