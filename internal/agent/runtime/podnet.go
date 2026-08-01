package runtime

import (
	"log/slog"
	"strings"
)

// PodNetworkMode classifies which CNI configuration the runtime
// container's entrypoint will write for this node. The modes are NOT
// interchangeable and the difference is invisible from the outside — a
// node in any of them joins and reports Ready — so this type exists to
// make the distinction nameable, loggable, and reportable rather than
// implicit in "is OUTPOST_POD_CIDR set".
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

	// PodNetworkPeerFlannel means this node joins a PEER-hosted control
	// plane and gets its pod network from stock k3s flannel VXLAN pinned
	// to the tailnet underlay (--flannel-iface=tailscale0). See
	// docs/adr-peer-dks-pod-network.md (Option A).
	//
	// This is a CORRECT multi-node mode, not a fallback: flannel reads
	// Node.spec.podCIDR — allocated by the peer's own k3s
	// controller-manager — so there is no per-node CIDR for outpost to
	// carry, no conflist for the entrypoint to template, and nothing for
	// cloudbox to allocate. The pod CIDR is genuinely unknown on this
	// side of the join, which is why PodCIDR stays empty here rather
	// than being filled with a placeholder that would read as a real
	// allocation.
	PodNetworkPeerFlannel PodNetworkMode = "peer-flannel"
)

// PodNetworkModeEnv is the env var runtime.Up stamps on the container so
// the entrypoint can branch on the classified mode instead of
// re-deriving it from "is OUTPOST_POD_CIDR set" — a test that no longer
// distinguishes peer-flannel from the single-node fallback.
const PodNetworkModeEnv = "OUTPOST_POD_NETWORK_MODE"

// FallbackPodCIDR mirrors the CNI_LOCAL_POD_CIDR default in
// image/entrypoint.sh — the fixed range the single-node fallback
// allocates from on EVERY node. Reported for operator legibility only;
// nothing here configures the container (the entrypoint owns that
// value, and an operator override rides in via ExtraEnv).
//
// MUST equal the entrypoint's default, and TestFallbackPodCIDRMatchesEntrypoint
// pins the two together. They drifted once already: this constant said
// 10.43.42.0/24 (inside the k3s SERVICE CIDR — the silent node-local
// misroute the entrypoint comment records as fixed) long after the
// entrypoint had moved to 10.42.255.0/24, so every status view and log
// line reported a range no container ever allocated from.
const FallbackPodCIDR = "10.42.255.0/24"

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

// ClassifyPodNetwork is the single source of truth for the mode.
//
// peerFlannel wins over the CIDR test: a peer-joined worker's pod CIDR
// is allocated by the PEER's k3s controller-manager and is not knowable
// here, so "PodCIDR is empty" no longer implies the single-node
// fallback the way it did when cloudbox was the only allocator.
func ClassifyPodNetwork(podCIDR string, peerFlannel bool) PodNetwork {
	if peerFlannel {
		// PodCIDR intentionally empty — flannel reads Node.spec.podCIDR.
		return PodNetwork{Mode: PodNetworkPeerFlannel}
	}
	if cidr := strings.TrimSpace(podCIDR); cidr != "" {
		return PodNetwork{Mode: PodNetworkOverlay, PodCIDR: cidr}
	}
	return PodNetwork{Mode: PodNetworkSingleNodeFallback, PodCIDR: FallbackPodCIDR}
}

// PodNetwork classifies the node these Options describe, honoring an
// ExtraEnv override of the fallback range so the reported CIDR matches
// what the container will really allocate from.
func (o Options) PodNetwork() PodNetwork {
	n := ClassifyPodNetwork(o.PodCIDR, o.PeerFlannel)
	if n.Mode != PodNetworkSingleNodeFallback {
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

// podNetworkEnvArgs renders the `-e` pair that tells the entrypoint which
// pod-network mode it is in.
//
// Returns NOTHING outside peer-flannel mode, on purpose: the entrypoint's
// pre-existing branches key off OUTPOST_POD_CIDR, and leaving the new
// variable unset is what keeps the cloudbox-hosted overlay and the
// single-node fallback rendering a byte-identical container command to the
// one that shipped before peer-hosted planes existed.
func podNetworkEnvArgs(opts Options) []string {
	if opts.PodNetwork().Mode != PodNetworkPeerFlannel {
		return nil
	}
	return []string{"-e", PodNetworkModeEnv + "=" + string(PodNetworkPeerFlannel)}
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
	if n.Mode == PodNetworkPeerFlannel {
		// Deliberately NOT the fallback WARN below: peer-flannel is a
		// correct multi-node mode, and crying wolf here would train
		// operators to ignore the one line that means real corruption.
		slog.Info("cluster: node joins a peer-hosted control plane with stock flannel VXLAN "+
			"over the tailnet underlay (--flannel-iface=tailscale0); the pod CIDR comes from "+
			"Node.spec.podCIDR, allocated by the peer's k3s controller-manager",
			"node", node, "mode", string(n.Mode))
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
