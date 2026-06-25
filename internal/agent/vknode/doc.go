// Package vknode is the per-outpost half of the cloudbox cluster: it
// joins a cloud-side Kubernetes API server as a virtual node and runs
// scheduled Pods as podman containers on the host.
//
// The package is split into three layers:
//
//   - Layer 1 (this file's siblings client.go / containers.go / images.go
//     / exec.go) — a thin HTTP-over-unix client for the local libpod
//     REST API. Deliberately does not depend on
//     github.com/containers/podman/v5/pkg/bindings because that pulls in
//     containers/storage (cgo), which would break outpost's
//     cross-compile story. We only need ~10 endpoints; a hand-rolled
//     client is smaller and cheaper to bump than the full SDK.
//
//   - Layer 2 (translate.go) — convert a corev1.Pod to a libpod
//     SpecGenerator. Stamps every container with the
//     outpost.io/managed=true label and the pod's namespace / name /
//     uid so reconnect reconciliation can find what we already own and
//     so the host operator can identify "who is running what" with a
//     plain `podman ps`.
//
//   - Layer 3 (provider.go, node.go) — implement
//     virtual-kubelet's PodLifecycleHandler and NodeProvider interfaces
//     on top of layers 1 and 2. The PodController and NodeController
//     from virtual-kubelet take care of the apiserver watch/list and
//     status-update plumbing.
//
// The cluster-side details (k3s in cloudbox, RBAC, sharing) live in the
// plan at ~/.claude/plans/pooling-podman-containers-registered-wit-steady-reef.md
// and in the separate cloudbox repo.
package vknode
