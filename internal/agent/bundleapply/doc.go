// Package bundleapply installs an app/bundle manifest set against a
// kubeconfig-addressed Kubernetes control plane — specifically a
// PEER-HOSTED one, with no cloudbox dependency anywhere on the execution
// path.
//
// # Why this exists
//
// The cloudbox-hosted DKS plane already has a working bundle-apply path,
// but that path assumes a cloud-minted kubeconfig and a cloud-side
// orchestrator. A peer plane is just k3s on someone's box: the operator
// has a kubeconfig (client-cert auth, written by k3s at
// /etc/rancher/k3s/k3s.yaml and copied per-user to
// ~/.kube/outpost-control-plane/k3s.yaml) and nothing else. This package
// takes that kubeconfig PATH as a parameter and applies a bundle using
// only client-go — no access_token, no /api/v1 call, no overlay
// credential fetch.
//
// # The two kubeconfigs are never conflated
//
//	peer control plane : ~/.kube/outpost-control-plane/k3s.yaml
//	cloudbox plane     : ~/.kube/outpost.yaml
//
// This package holds neither path as a default. The caller (the script,
// or admincore.BundleApply behind the MCP/CLI surfaces) passes the exact
// path; PeerControlPlaneKubeconfig /
// CloudboxKubeconfig are provided only as named references so no caller
// has to hardcode a literal and accidentally point the peer path at the
// cloudbox file.
//
// # Guarantees
//
//   - Ordering: namespaces (and other cluster-scoped prerequisites) are
//     applied before namespaced objects, so a bundle that ships its own
//     Namespace plus workloads in one set applies cleanly on a fresh
//     cluster.
//   - Idempotence: apply uses server-side apply semantics (a stable
//     field manager), so applying the same bundle twice converges instead
//     of duplicating or erroring.
//   - Readiness: an apply is NOT done when the apiserver accepts the
//     object — it is done when the workload reports Ready. WaitForReady
//     blocks up to an explicit timeout and returns a non-nil error (which
//     the operator entry point turns into a non-zero exit) on timeout.
//
// # Evidence invariant (docs/fleet-evidence-invariant.md)
//
// No success state is ever reached by the ABSENCE of evidence. An empty
// bundle, an unreachable apiserver, an object that never becomes Ready, a
// DaemonSet that schedules onto zero nodes — every one of these FAILS
// loudly. Default-deny is the rule on any unparseable or absent signal.
//
// # Wiring status
//
// This package is deliberately NOT wired into main.go / conf / admincore
// / MCP / the CLI. It exposes a clean Go API plus the standalone script
// script/dks-peer-bundle-apply.sh. See docs/peer-dks-bundle-apply.md
// ("Deferred wiring") for the exact four-surface hooks a follow-up
// integration run should add.
package bundleapply

// PeerControlPlaneKubeconfig is the conventional per-user path to a
// peer-hosted (k3s) control plane's admin kubeconfig. It is NOT applied
// as a default anywhere in this package — callers pass an explicit path.
// It exists so a caller can reference the peer plane by name rather than
// retyping the literal and risking a typo that lands on the cloudbox
// file.
const PeerControlPlaneKubeconfig = "~/.kube/outpost-control-plane/k3s.yaml"

// CloudboxKubeconfig is the conventional per-user path to the
// cloudbox-hosted plane's kubeconfig. Named here purely so callers can
// assert "this is NOT the peer path" without hardcoding the literal.
const CloudboxKubeconfig = "~/.kube/outpost.yaml"
