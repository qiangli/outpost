package runtime

import (
	"log/slog"
	"strings"
)

// PodNetworkMode classifies which of the two CNI configurations the
// runtime container's entrypoint will write for this node. The two
// modes are NOT interchangeable and the difference is invisible from
// the outside — a node in either mode joins and reports Ready — so
// this type exists to make the distinction nameable, loggable, and
// reportable rather than implicit in "is OUTPOST_POD_CIDR set".
type PodNetworkMode string

const (
	// PodNetworkOverlay means cloudbox carved a per-outpost pod CIDR
	// for this node and the entrypoint writes the outpost-cni conflist
	// over the tailscale overlay: unique pod IPs per node, cross-node
	// pod routing. This is the only mode that is correct in a
	// multi-node cluster.
	PodNetworkOverlay PodNetworkMode = "overlay"

	// PodNetworkSingleNodeFallback means no pod CIDR was allocated, so
	// the entrypoint falls back to a plain bridge + host-local IPAM out
	// of a FIXED range that is identical on every node. Correct for a
	// single-node cluster; catastrophic in a multi-node one — every
	// node hands out the same pod IPs, Service endpoint lists contain
	// duplicate addresses, and kube-proxy can DNAT a request to the
	// wrong local workload. Nothing errors and the node reports Ready,
	// which is precisely why it has to be announced.
	PodNetworkSingleNodeFallback PodNetworkMode = "single-node-fallback"
)

// FallbackPodCIDR mirrors the CNI_LOCAL_POD_CIDR default in
// image/entrypoint.sh — the fixed range the single-node fallback
// allocates from on EVERY node. Reported for operator legibility only;
// nothing here configures the container (the entrypoint owns that
// value, and an operator override rides in via ExtraEnv).
const FallbackPodCIDR = "10.43.42.0/24"

// fallbackCIDREnv is the entrypoint's operator override for the
// fallback range. Options.ExtraEnv is the only way it can be set from
// the outpost side, so PodNetwork honors it when reporting the
// effective CIDR.
const fallbackCIDREnv = "CNI_LOCAL_POD_CIDR"

// PodNetwork is the classified pod-network state of one node: which
// mode the container will come up in, and the pod CIDR that mode will
// actually allocate from.
type PodNetwork struct {
	// Mode is the classification. Never empty.
	Mode PodNetworkMode

	// PodCIDR is the range pods on this node get IPs from. In overlay
	// mode it is the cloudbox-allocated per-node CIDR; in fallback mode
	// it is the fixed range shared with every other node.
	PodCIDR string
}

// Overlay reports whether this node has a real (per-node, routable)
// pod network.
func (n PodNetwork) Overlay() bool { return n.Mode == PodNetworkOverlay }

// ClassifyPodNetwork is the single source of truth for the mode. A
// non-empty pod CIDR means the overlay conflist; an empty one means
// the shared-range fallback.
func ClassifyPodNetwork(podCIDR string) PodNetwork {
	if cidr := strings.TrimSpace(podCIDR); cidr != "" {
		return PodNetwork{Mode: PodNetworkOverlay, PodCIDR: cidr}
	}
	return PodNetwork{Mode: PodNetworkSingleNodeFallback, PodCIDR: FallbackPodCIDR}
}

// PodNetwork classifies the node these Options describe, honoring an
// ExtraEnv override of the fallback range so the reported CIDR matches
// what the container will really allocate from.
func (o Options) PodNetwork() PodNetwork {
	n := ClassifyPodNetwork(o.PodCIDR)
	if n.Overlay() {
		return n
	}
	for _, kv := range o.ExtraEnv {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k == fallbackCIDREnv && strings.TrimSpace(v) != "" {
			n.PodCIDR = strings.TrimSpace(v)
		}
	}
	return n
}

// Log announces the pod-network mode at boot. The fallback is logged
// at WARN, not Info: a node with no pod network is a silent
// multi-node-cluster corruption, and this line is the only place it
// becomes visible before pods start colliding. The overlay case logs
// at Info with the CIDR so the two are trivially greppable.
func (n PodNetwork) Log(node string) {
	if n.Overlay() {
		slog.Info("cluster: node has an overlay pod network",
			"node", node, "mode", string(n.Mode), "pod_cidr", n.PodCIDR)
		return
	}
	slog.Warn("cluster: node has NO pod network — single-node fallback CNI "+
		"(fixed pod CIDR, identical on every node). Safe ONLY as a single-node "+
		"cluster: in a multi-node cluster pod IPs collide across nodes, Service "+
		"endpoints contain duplicate addresses, and kube-proxy can route to the "+
		"wrong workload. Multi-node requires the cloudbox overlay pod CIDR "+
		"(re-pair to have cloudbox allocate one).",
		"node", node, "mode", string(n.Mode), "pod_cidr", n.PodCIDR)
}
