// Package nodeaddr makes an apiserver able to reach kubelets that are
// only reachable through a tunnel.
//
// WHY THIS EXISTS. The apiserver dials kubelet using the address and port
// it finds in Node.Status.Addresses + Status.DaemonEndpoints.KubeletEndpoint.
// When kubelet runs on a machine reachable only through a tunnel, those
// self-reported defaults (the node's own LAN IP : 10250) do not resolve
// from the control plane. Every `kubectl logs`, `exec`, `port-forward`
// and metrics scrape depends on that dial, so without this the cluster
// looks healthy — nodes go Ready, pods schedule — and the operator
// commands people actually use are the ones that fail.
//
// This is the peer-hosted counterpart of the same job cloudbox does for
// the cloud-hosted control plane. It moves here because a control plane
// running on a user's own machine has no cloudbox in the loop
// (dhnt/docs/dks-control-plane-on-sphere.md).
package nodeaddr

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Node-identity label vocabulary, matching the cloud-hosted control plane
// and the peer packages (nodegc, nodecap) exactly. A virtual-kubelet node
// carries RuntimeLabel=RuntimeVirtual.
const (
	RuntimeLabel   = "outpost.dhnt.io/runtime"
	RuntimeVirtual = "virtual"
)

// IsVirtualNode reports whether n is a virtual-kubelet node.
//
// Address reconciliation exists to make the apiserver dial a kubelet that is
// only reachable through a tunnel. A virtual-kubelet node runs NO kubelet of
// its own — it serves only the Pod lifecycle subset, not the streaming API —
// so there is nothing at the derived loopback address:port to dial. Patching
// one would publish an ExternalIP the apiserver then prefers for `kubectl
// logs`/`exec`/`port-forward`/metrics, turning "unsupported on a virtual
// node" into a confusing dial timeout, and would consume a slot in k3s's
// ExternalIP routing trie that a real kubelet could use. So the reconciler
// skips virtual nodes by LABEL — never by node name.
func IsVirtualNode(n *corev1.Node) bool {
	return n != nil && n.Labels[RuntimeLabel] == RuntimeVirtual
}

// LoopbackForNode maps a node name to a stable, node-UNIQUE address in
// 127.0.0.0/8.
//
// UNIQUENESS IS LOAD-BEARING, NOT COSMETIC, and it is the whole reason
// this function is not just "return 127.0.0.1". k3s's tunnel server
// indexes every node by its InternalIP *and* ExternalIP in a cidranger
// trie, and that trie's insert is last-writer-wins for an equal network.
// So if every node advertises ExternalIP=127.0.0.1 they collapse into a
// SINGLE 127.0.0.1/32 entry, and the backend dialer resolves every
// kubelet dial to one arbitrary node — tunnelling the request into the
// wrong node's session, which then fails to dial a kubelet port that is
// not listening there and drops the session. At most one node ever has
// working logs/exec, and WHICH one is nondeterministic.
//
// Derived from the node NAME, not the kubelet port: names are stable
// across restarts and independent of port-allocation state, which has
// historically contained duplicates.
//
// Collisions resolve by linear probe over `taken`. Callers MUST probe in
// a deterministic order (sorted names) so a given node set always yields
// the same mapping — otherwise addresses churn between passes and every
// pass issues pointless patches.
func LoopbackForNode(name string, taken map[string]string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	// Offset past 127.0.0.0/24 so this can never mint 127.0.0.1 (the
	// tunnel's own bind address and the apiserver's advertise address)
	// or 127.0.0.0. Slots 256..65535 => 127.0.1.0 … 127.0.255.255.
	const slots = 65536 - 256
	slot := int(h.Sum32()%uint32(slots)) + 256
	for i := 0; i < slots; i++ {
		s := 256 + (slot-256+i)%slots
		addr := fmt.Sprintf("127.0.%d.%d", s>>8, s&0xff)
		if owner, used := taken[addr]; !used || owner == name {
			taken[addr] = name
			return addr
		}
	}
	return "" // 65k nodes; unreachable in practice
}

// Kubelet port range. Chosen to avoid three things that would collide:
// the well-known/registered ports below 1024, Kubernetes' NodePort range
// (30000-32767), and Linux's default ephemeral range (32768-60999) which
// outbound sockets draw from. 10000 slots is far more than a personal
// cluster needs.
const (
	kubeletPortBase = 21000
	// 21000..29999 — stops SHORT of NodePort (30000). An earlier value of
	// 10000 slots ran to 30999 and overlapped it; the test caught it.
	kubeletPortSlots = 9000
)

// KubeletPortForNode derives the loopback port on which a node's kubelet
// is published through the tunnel.
//
// DERIVED, NOT ALLOCATED, and that is the point: both ends compute it
// independently from the node name, so nothing has to distribute it. The
// control-plane host needs the number to patch DaemonEndpoints; the node
// needs it to bind kubelet and to name its frpc proxy's remotePort. A
// registry, a handshake, or a config push would each add a way for the
// two to disagree — and they must not, because the tunnel publishes
// remotePort == localPort (see the runtime image's entrypoint).
//
// Cloudbox instead ALLOCATES this per host at pairing time
// (ClusterConfig.KubeletProxyPort). That is the right design when a
// central party already mediates every join and can hold the pool. A
// peer-hosted plane has no such party, so derivation replaces it.
//
// A collision — two node names hashing to one slot — fails LOUD rather
// than silently misrouting: frp refuses the second proxy's remotePort and
// that node's kubelet simply is not published, which surfaces as its
// `kubectl logs` failing rather than as another node's logs being served.
// At a handful of nodes over 10000 slots the probability is negligible,
// and the failure mode is the safe one.
func KubeletPortForNode(name string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte("kubelet:" + name))
	return kubeletPortBase + int(h.Sum32()%uint32(kubeletPortSlots))
}

// DerivedKubeletPort is the KubeletPortFunc that pairs with
// KubeletPortForNode — the default for a peer-hosted control plane.
func DerivedKubeletPort(nodeName string) (int, bool) {
	if nodeName == "" {
		return 0, false
	}
	return KubeletPortForNode(nodeName), true
}

// KubeletPortFunc reports the loopback port on which THIS control-plane
// host can reach the named node's kubelet — i.e. the local end of
// whatever tunnel carries it. Returning ok=false skips the node, which is
// the right behaviour for one whose tunnel is not up yet.
type KubeletPortFunc func(nodeName string) (port int, ok bool)

// Reconciler patches node addresses so the apiserver can reach kubelets.
type Reconciler struct {
	Client   kubernetes.Interface
	PortFor  KubeletPortFunc
	Interval time.Duration
	Log      *slog.Logger
}

// Run reconciles until ctx is cancelled.
//
// Polled rather than watched: the patch is idempotent, so a missed event
// costs one interval of staleness and nothing else. A watch would add a
// failure mode (silently dead informer) for no benefit at this scale.
//
// LOGGING IS PART OF THE CONTRACT HERE, not decoration. Every reconcile
// failure used to be logged at Debug, which is below the daemon's default
// level — so a reconciler that was running and failing every single pass
// emitted NOTHING, and was indistinguishable in the logs from one that had
// never started. That ambiguity is what made a control plane with no
// ExternalIP on any node take far longer to diagnose than it should have. So:
// one Info line on start (the positive evidence "it is running"), and Warn on
// the transition into failure and on recovery (the negative evidence), with
// the steady-state repeats kept at Debug so a long outage cannot flood the log.
func (r *Reconciler) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	log.Info("nodeaddr: reconciler started", "interval", interval)
	setStatus(func(s *Status) { s.Started = true })

	fails := 0
	pass := func() {
		err := r.Once(ctx)
		if err == nil {
			if fails > 0 {
				log.Warn("nodeaddr: reconcile recovered", "after_failures", fails)
			}
			fails = 0
			setStatus(func(s *Status) {
				s.LastRunAt = time.Now()
				s.LastError = ""
				s.ConsecutiveFailures = 0
			})
			return
		}
		fails++
		if fails == 1 {
			log.Warn("nodeaddr: reconcile failed — the apiserver cannot reach kubelets "+
				"until this succeeds (kubectl logs/exec/top will fail)", "err", err)
		} else {
			log.Debug("nodeaddr: reconcile still failing", "err", err, "consecutive", fails)
		}
		setStatus(func(s *Status) {
			s.LastRunAt = time.Now()
			s.LastError = err.Error()
			s.ConsecutiveFailures = fails
		})
	}

	pass()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		}
	}
}

// Once performs a single reconcile pass.
func (r *Reconciler) Once(ctx context.Context) error {
	if r.Client == nil || r.PortFor == nil {
		return fmt.Errorf("nodeaddr: Client and PortFor are required")
	}
	log := r.Log
	if log == nil {
		log = slog.Default()
	}

	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	nodes, err := r.Client.CoreV1().Nodes().List(listCtx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	// Sort by name: LoopbackForNode probes against `taken`, so a stable
	// visit order is what makes the mapping reproducible. The apiserver
	// does not promise a stable List order.
	items := make([]*corev1.Node, 0, len(nodes.Items))
	for i := range nodes.Items {
		items = append(items, &nodes.Items[i])
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	taken := map[string]string{}
	for _, n := range items {
		if IsVirtualNode(n) {
			continue // no kubelet to reach — see IsVirtualNode
		}
		addr := LoopbackForNode(n.Name, taken)
		if addr == "" {
			continue
		}
		port, ok := r.PortFor(n.Name)
		if !ok || port <= 0 {
			continue // tunnel not up for this node yet; try next pass
		}
		if hasAddr(n, addr) && n.Status.DaemonEndpoints.KubeletEndpoint.Port == int32(port) {
			continue // already correct — do not churn the API
		}
		pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
		err := r.patch(pctx, n.Name, addr, port)
		pcancel()
		if err != nil {
			log.Warn("nodeaddr: patch failed", "node", n.Name, "err", err)
			continue
		}
		log.Info("nodeaddr: patched", "node", n.Name, "externalIP", addr, "kubeletPort", port)
	}
	return nil
}

// hasAddr reports whether the node already advertises exactly addr as its
// ExternalIP. The comparison is exact on purpose: a node still carrying a
// shared 127.0.0.1, or another node's address, is precisely the bug this
// package exists to fix and must be re-patched.
func hasAddr(n *corev1.Node, addr string) bool {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeExternalIP {
			return a.Address == addr
		}
	}
	return false
}

// patch writes a strategic-merge patch against the node's status
// subresource.
//
// Strategic merge rather than a JSON patch because Addresses has a
// list-key (Type) the API understands, so the ExternalIP entry is
// replaced without disturbing the Hostname/InternalIP entries kubelet
// manages.
//
// ExternalIP rather than InternalIP is the load-bearing choice: kubelet
// overwrites InternalIP every ~10s and outright rejects a loopback value
// there ("nodeIP can't be loopback address"), but it does not manage
// ExternalIP — that is cloud-controller territory — so this patch
// sticks.
//
// The apiserver and metrics-server both run with
// --kubelet-preferred-address-types=ExternalIP — ExternalIP ONLY, with no
// InternalIP/Hostname fallback, and that is not an oversight to be repaired
// the next time a scrape fails. The address this reconciler publishes is the
// only one reachable from the control plane: it is the local end of the
// authenticated FRP tunnel. A node's InternalIP (e.g. an overlay 100.64.x.x)
// and its Hostname resolve to paths that either do not route from the
// apiserver's network namespace or bypass the tunnel's authentication
// entirely. Appending a fallback therefore does not fix a missing ExternalIP;
// it converts the loud, accurate failure "no address matched types
// [ExternalIP]" into a silent dial against an address that cannot serve, while
// weakening the trust boundary. When that error appears, the fix is upstream of
// here: find out why the reconciler is not running (see cmd/outpost's
// startControlPlaneReconcilers) — not to widen the list.
func (r *Reconciler) patch(ctx context.Context, name, addr string, port int) error {
	patch := map[string]any{
		"status": map[string]any{
			"addresses": []map[string]any{
				{"type": "ExternalIP", "address": addr},
			},
			"daemonEndpoints": map[string]any{
				"kubeletEndpoint": map[string]any{"Port": port},
			},
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = r.Client.CoreV1().Nodes().Patch(
		ctx, name, types.StrategicMergePatchType, body, metav1.PatchOptions{}, "status",
	)
	return err
}

// NodeAddressDiagnostic provides deterministic, secret-free diagnostics for node addressing status.
type NodeAddressDiagnostic struct {
	NodeName            string `json:"node_name"`
	ExpectedExternalIP  string `json:"expected_external_ip,omitempty"`
	ObservedExternalIP  string `json:"observed_external_ip,omitempty"`
	ExpectedKubeletPort int    `json:"expected_kubelet_port,omitempty"`
	ObservedKubeletPort int    `json:"observed_kubelet_port,omitempty"`
	Patched             bool   `json:"patched"`
}

// Diagnostics returns deterministic node addressing status for all nodes in the cluster.
func (r *Reconciler) Diagnostics(ctx context.Context) ([]NodeAddressDiagnostic, error) {
	if r.Client == nil || r.PortFor == nil {
		return nil, fmt.Errorf("nodeaddr: Client and PortFor are required")
	}

	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	nodes, err := r.Client.CoreV1().Nodes().List(listCtx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	items := make([]*corev1.Node, 0, len(nodes.Items))
	for i := range nodes.Items {
		items = append(items, &nodes.Items[i])
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	taken := map[string]string{}
	var diagnostics []NodeAddressDiagnostic
	for _, n := range items {
		if IsVirtualNode(n) {
			continue // virtual nodes are not address-reconciled — see IsVirtualNode
		}
		expectedAddr := LoopbackForNode(n.Name, taken)
		expectedPort, ok := r.PortFor(n.Name)
		if !ok || expectedPort <= 0 {
			expectedPort = 0
		}

		var observedAddr string
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeExternalIP {
				observedAddr = a.Address
				break
			}
		}
		observedPort := int(n.Status.DaemonEndpoints.KubeletEndpoint.Port)

		patched := expectedAddr != "" && expectedPort > 0 &&
			observedAddr == expectedAddr && observedPort == expectedPort

		diagnostics = append(diagnostics, NodeAddressDiagnostic{
			NodeName:            n.Name,
			ExpectedExternalIP:  expectedAddr,
			ObservedExternalIP:  observedAddr,
			ExpectedKubeletPort: expectedPort,
			ObservedKubeletPort: observedPort,
			Patched:             patched,
		})
	}
	return diagnostics, nil
}
