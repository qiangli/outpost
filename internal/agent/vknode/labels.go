package vknode

// Well-known container labels vknode stamps onto every container it
// creates. Reconcile uses ManagedLabel as the boundary between
// outpost-cluster-owned containers and everything else the user runs
// locally with podman — we never touch a container that lacks it.
//
// PodUIDLabel / PodNamespaceLabel / PodNameLabel let the host operator
// answer "who is running what on my machine" with a plain `podman ps
// --format '{{.Labels}}'`; they also let reconcile look up an owning
// pod when our BoltDB mapping is stale (e.g. after a vk crash that
// missed a write).
const (
	ManagedLabel      = "outpost.io/managed"
	PodUIDLabel       = "outpost.io/pod-uid"
	PodNameLabel      = "outpost.io/pod-name"
	PodNamespaceLabel = "outpost.io/pod-namespace"

	// ClusterLabel scopes a managed container (or volume) to the CLUSTER
	// that created it, not just to "some outpost vk". Two providers can
	// share one podman socket while serving different control planes
	// (e.g. the supervised daemon on cloud DKS plus a standalone
	// outpost-vk on a peer DKS); without this label each one's reconcile
	// saw — and garbage-collected — the other's containers. The value is
	// derived from the cluster CA fingerprint (see ClusterIdentityFromCA),
	// NOT the API URL, so it stays stable when the same cluster is
	// reached via different addresses (LAN IP vs mesh forward).
	//
	// Legacy containers created before this label existed carry only
	// ManagedLabel. The migration rule is fail-closed: an unlabeled
	// container is never listed, adopted, or deleted from reconcile —
	// only an apiserver-driven pod-UID match (UIDs are apiserver-minted
	// UUIDs, so a match is unambiguous proof of ownership) lets a scoped
	// provider adopt or delete one.
	ClusterLabel = "outpost.io/cluster"

	// ContainerNameLabel records the K8s container-spec name inside a
	// (potentially multi-container) Pod. v1 only supports
	// single-container Pods so this is always pod.Spec.Containers[0].Name,
	// but recording it now makes the multi-container expansion in a
	// future version a pure-additive change.
	ContainerNameLabel = "outpost.io/container-name"
)

// Well-known Node labels vknode stamps or accepts through RunOptions.
// These are Kubernetes scheduling surface, so keep names centralized
// instead of sprinkling string literals through callers.
const (
	NodeHostLabel          = "outpost.dhnt.io/host"
	NodeLocalityLANLabel   = "outpost.dhnt.io/lan-group"
	NodeLocalityTierLabel  = "outpost.dhnt.io/tier"
	NodeLocalityTierTP     = "tp"
	NodeLocalityTierLAN    = "lan"
	NodeLocalityTierWAN    = "wan"
	NodeLocalityTierRemote = "remote"
)

// NodeLocalityLabels returns the non-empty locality labels for a Node.
// Values are normalized to Kubernetes label-value syntax; empty or fully
// trimmed values are omitted. Callers should only pass measured or
// cloudbox-issued locality data; per-host names are not a LAN group.
func NodeLocalityLabels(lanGroup, tier string) map[string]string {
	labels := map[string]string{}
	if v := sanitizeLabelValue(lanGroup); v != "" {
		labels[NodeLocalityLANLabel] = v
	}
	if v := sanitizeLabelValue(tier); v != "" {
		labels[NodeLocalityTierLabel] = v
	}
	return labels
}
