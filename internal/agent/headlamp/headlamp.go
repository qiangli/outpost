// Package headlamp deploys and audits a Headlamp operating UI on a
// peer-hosted DKS control plane.
//
// WHY. cloudbox's Cluster page is a cross-host INVENTORY only — it
// structurally cannot operate a peer-hosted plane (its endpoint is not
// even registered when cloudbox runs no cluster). Operating a cluster
// belongs where the kubeconfig is: the control-plane host. Headlamp is
// that operating UI, and it is also a full cluster-admin surface, so
// the auth boundary is designed before the deployment shape.
//
// SECURITY MODEL — three layers, none optional:
//
//  1. The Headlamp pod's own ServiceAccount receives ZERO RBAC grants.
//     Headlamp runs with token login: every apiserver request is
//     authenticated with the token the operator pastes into the UI for
//     that session. A compromised Headlamp pod therefore holds only an
//     unprivileged SA token (system:basic-user discovery), never
//     cluster-admin. The token mount stays on solely so Headlamp's
//     in-cluster config (apiserver address + CA) resolves.
//  2. The Service is ClusterIP only — never NodePort, never
//     LoadBalancer, never hostNetwork/hostPort. The only way off the
//     node is a port-forward pinned to 127.0.0.1 (ForwardArgs), which
//     follows outpost's standing rule that loopback + the tunnel/mesh
//     is the only ingress. Remote reach rides the EXISTING
//     authenticated surfaces — the mesh forwarder (allowlisted service
//     name MeshServiceName) or a matrix-tunnel app with RequireLogin —
//     not a new port. No LAN bind is offered here; a follow-up that
//     offers one must force the auth gate per the admin_addr precedent.
//  3. An OPTIONAL read-only viewer ServiceAccount (ViewerServiceAccount)
//     is bound to the built-in aggregate ClusterRole "view" (namespaced
//     reads, excludes Secrets and RBAC objects) plus a dedicated
//     nodes-get/list/watch ClusterRole (nodes are cluster-scoped, so
//     "view" alone cannot show them, and a plane UI without node
//     visibility is pointless). It exists only so the operator can mint
//     a scoped audit token to paste at login; no pod mounts it. That
//     token is a cluster credential — per the tenancy model it must
//     never reach a guest or non-owner (guests reach shared apps via
//     Periscope); see Options.DisableViewerRBAC for the full caveat.
//     Set Options.DisableViewerRBAC to converge it away. Admin access is
//     always the operator pasting an admin token themselves — this
//     package never mints or persists one.
//
// Posture drift (a Service flipped to NodePort, a binding granted to
// the pod SA, a hostNetwork pod) is detected by Inspect and repaired by
// re-running Deploy.
//
// WIRING IS DEFERRED. No four-surface toggle exists yet; the
// headlamp_enabled / headlamp_port follow-up (loom/zot builtin pattern)
// is described in docs/peer-dks-headlamp.md. Until then the operator
// path is: Deploy (or `kubectl apply` of RenderJSON output), ForwardArgs
// for the loopback forward, and `outpost mesh service add headlamp
// 127.0.0.1:<port>` for authenticated remote reach.
//
// Headlamp against a real peer-hosted plane is HARDWARE-UNPROVEN — see
// docs/peer-dks-headlamp.md for the evidence boundary.
package headlamp

import (
	"fmt"
	"net"
	"regexp"
)

const (
	// DefaultNamespace is the dedicated namespace everything lives in.
	// Dedicated so Remove can delete it without collateral damage.
	DefaultNamespace = "headlamp"

	// DefaultName names the ServiceAccount, Deployment, and Service.
	// The app.kubernetes.io/name label matches what the merged
	// acceptance harness (script/dks-peer-acceptance.sh check 9)
	// already selects on.
	DefaultName = "headlamp"

	// DefaultImage pins the upstream Headlamp release this package was
	// written against. Override via Options.Image; the pin has NOT been
	// validated against a live peer plane (hardware-unproven).
	DefaultImage = "ghcr.io/headlamp-k8s/headlamp:v0.27.0"

	// DefaultLocalPort is the loopback port ForwardArgs publishes on.
	// script/dks-headlamp-verify.sh probes the same default.
	DefaultLocalPort = 18466

	// ServicePort is the ClusterIP Service port; containerPort is what
	// headlamp-server itself listens on inside the pod.
	ServicePort   = 80
	containerPort = 4466

	// MeshServiceName is the forwarder service name the follow-up
	// wiring (and today's manual `outpost mesh service add`) exposes
	// the loopback forward under. The mesh forwarder only reaches
	// allowlisted names — that boundary is the remote auth gate.
	MeshServiceName = "headlamp"

	// ViewerServiceAccount is the optional read-only token-minting SA.
	ViewerServiceAccount = "headlamp-viewer"

	// NodeViewClusterRole grants get/list/watch on nodes only — the one
	// cluster-scoped read "view" lacks that a plane UI needs.
	NodeViewClusterRole = "outpost-headlamp-nodeview"

	// viewerBindingView / viewerBindingNodes are the two
	// ClusterRoleBindings attached to ViewerServiceAccount. They are
	// the ONLY bindings Inspect accepts for it.
	viewerBindingView  = "outpost-headlamp-viewer-view"
	viewerBindingNodes = "outpost-headlamp-viewer-nodeview"

	// managedByLabel marks every object this package owns; Remove
	// refuses to delete a namespace that does not carry it.
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "outpost"
	nameLabel      = "app.kubernetes.io/name"
	partOfLabel    = "app.kubernetes.io/part-of"
	partOfValue    = "dks"
)

// Options parameterizes a Headlamp deployment. The zero value is valid
// and maps to the secure defaults above.
type Options struct {
	Namespace string // "" => DefaultNamespace
	Name      string // "" => DefaultName
	Image     string // "" => DefaultImage
	LocalPort int    // 0 => DefaultLocalPort (loopback forward port)

	// DisableViewerRBAC skips (and on Deploy, removes) the read-only
	// viewer SA and its two bindings. Inverted so the zero value keeps
	// the documented default: viewer RBAC on.
	//
	// OPERATOR CAUTION — a token minted from the viewer SA is a CLUSTER
	// CREDENTIAL: read-only, but cluster-wide. The tenancy model
	// (docs/dks-tenancy-model.md) is absolute here: non-owners get no
	// cluster credential; guests reach shared apps via Periscope. A
	// minted viewer token must therefore NEVER reach a guest or
	// non-owner — never pasted into a Headlamp session anyone but the
	// owner can drive, never handed out, never persisted. Mint it
	// short-lived (`kubectl -n headlamp create token headlamp-viewer
	// --duration=1h`), use it, let it expire.
	//
	// Default evaluated: ON remains the default because creating the SA
	// and its bindings confers no credential by itself — a credential
	// exists only once someone already holding cluster-admin mints a
	// token, which is an owner-only act. The residual risk is token
	// distribution, governed by the caution above and by
	// docs/peer-dks-headlamp.md; operators who want the minting target
	// itself gone set this flag.
	DisableViewerRBAC bool
}

// dns1123Label is the shape Kubernetes requires of namespace and
// resource names.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// withDefaults returns o with empty fields filled in.
func (o Options) withDefaults() Options {
	if o.Namespace == "" {
		o.Namespace = DefaultNamespace
	}
	if o.Name == "" {
		o.Name = DefaultName
	}
	if o.Image == "" {
		o.Image = DefaultImage
	}
	if o.LocalPort == 0 {
		o.LocalPort = DefaultLocalPort
	}
	return o
}

// Normalize fills defaults and validates. Every entry point
// (Deploy/Remove/Inspect/RenderJSON/ForwardArgs) goes through it.
func (o Options) Normalize() (Options, error) {
	o = o.withDefaults()
	if !dns1123Label.MatchString(o.Namespace) || len(o.Namespace) > 63 {
		return o, fmt.Errorf("headlamp: invalid namespace %q (want DNS-1123 label)", o.Namespace)
	}
	if !dns1123Label.MatchString(o.Name) || len(o.Name) > 63 {
		return o, fmt.Errorf("headlamp: invalid name %q (want DNS-1123 label)", o.Name)
	}
	if o.Image == "" {
		return o, fmt.Errorf("headlamp: image must not be empty")
	}
	if o.LocalPort < 1024 || o.LocalPort > 65535 {
		return o, fmt.Errorf("headlamp: local port %d out of range [1024,65535]", o.LocalPort)
	}
	return o, nil
}

// ValidateListenAddr enforces the loopback-only bind rule on a
// host:port the follow-up wiring (or any caller) intends to listen on.
// It rejects empty hosts (an empty host means all interfaces) and any
// non-loopback address. There is deliberately no override parameter: a
// future LAN bind must be a conscious follow-up that forces an auth
// gate on every request (the admin_addr precedent), not a flag here.
func ValidateListenAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("headlamp: listen addr %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("headlamp: listen addr %q binds all interfaces; loopback only", addr)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("headlamp: listen addr %q: host is not a literal IP or localhost; loopback only", addr)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("headlamp: listen addr %q is not loopback; the mesh/tunnel is the only remote ingress", addr)
	}
	return nil
}

// ForwardArgs returns the argv of the kubectl port-forward that
// publishes the ClusterIP Service on 127.0.0.1:<LocalPort>. The
// --address pin is the load-bearing part: the forward must never bind
// a non-loopback interface. kubeconfigPath may be empty to use
// kubectl's own resolution.
func ForwardArgs(kubeconfigPath string, o Options) ([]string, error) {
	o, err := o.Normalize()
	if err != nil {
		return nil, err
	}
	args := []string{"kubectl"}
	if kubeconfigPath != "" {
		args = append(args, "--kubeconfig", kubeconfigPath)
	}
	args = append(args,
		"-n", o.Namespace,
		"port-forward",
		"--address", "127.0.0.1",
		"svc/"+o.Name,
		fmt.Sprintf("%d:%d", o.LocalPort, ServicePort),
	)
	return args, nil
}
