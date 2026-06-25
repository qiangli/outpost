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

	// ContainerNameLabel records the K8s container-spec name inside a
	// (potentially multi-container) Pod. v1 only supports
	// single-container Pods so this is always pod.Spec.Containers[0].Name,
	// but recording it now makes the multi-container expansion in a
	// future version a pure-additive change.
	ContainerNameLabel = "outpost.io/container-name"
)
