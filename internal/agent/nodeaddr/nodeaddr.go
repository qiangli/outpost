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
func (r *Reconciler) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	if err := r.Once(ctx); err != nil {
		log.Debug("nodeaddr: initial reconcile failed (will retry)", "err", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Once(ctx); err != nil {
				log.Debug("nodeaddr: reconcile failed", "err", err)
			}
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
// sticks. The apiserver must run with
// --kubelet-preferred-address-types=ExternalIP,InternalIP,... so it
// prefers this over the unreachable address kubelet reports.
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
