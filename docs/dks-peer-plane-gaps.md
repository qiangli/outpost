# Peer-Hosted DKS Control Plane Gaps

Audit and closure tracking for peer-hosted DKS control-plane functionality.

## Gap inventory

| Item | Component | Implementation status | Live two-host proof |
| --- | --- | --- | --- |
| 1 | Peer control-plane boot | Closed | Pending |
| 2 | Worker tunnel and visitor | Closed | Pending |
| 3 | Node address and kubelet port patching | Closed | Pending |
| 4 | Stale node garbage collection | Closed | Pending |
| 5 | Peer join runtime selection | Closed | Pending |
| 6 | Pod networking and PodCIDR allocation | Closed: stock k3s flannel VXLAN over `tailscale0`; Kubernetes allocates `Node.spec.podCIDR` | Pending |
| 7 | Kubelet metrics and `kubectl top` | Closed in code | Pending corrected live gate |
| 8 | Runtime capability probe | Closed in code | Pending |

“Closed in code” means the implementation and deterministic tests exist. It does
not claim that the current revision has passed the two-host hardware gate.

## Item 6: pod networking and PodCIDRs

Peer DKS does not need a cloudbox-specific pod network or a second allocator.
The peer control plane starts stock k3s flannel VXLAN on the Tailscale underlay
with `--flannel-iface=tailscale0`. Kubernetes assigns each node its
`Node.spec.podCIDR`. Acceptance checks require distinct PodCIDRs and
cross-node pod reachability; they do not replace Kubernetes allocation.

## Item 7: kubelet metrics

The live gate disproved the earlier HelmChartConfig and worker `hostNetwork`
approaches: k3s 1.36 ships metrics-server as raw packaged manifests, and a
host-network pod scheduled on a worker receives that worker's namespace, not
the control-plane container namespace where frps publishes kubelets.

The corrected implementation keeps `k3s server --disable-agent` and disables
only k3s's packaged metrics-server Deployment. The runtime image installs the
official checksum-pinned v0.8.1 binary (the version embedded by k3s 1.36.1),
then supervises it as a non-root sibling of k3s and frps. A selectorless,
headless Service plus
Endpoints/EndpointSlice directs the aggregation layer to the container's
private address. K3s's endpoint-based aggregator routing reaches that process
without kube-proxy or a local kubelet.

The security boundary stays the same as logs/exec:

- metrics-server scrapes the `nodeaddr`-derived `127.0.x.y:<port>` endpoints,
  which exist only inside the control-plane container and are backed by
  token-authenticated worker frpc sessions;
- both kube-apiserver and metrics-server accept only the reconciler-owned
  `ExternalIP` for kubelet traffic, while k3s runs in `egress-selector-mode=pod`;
  neither silently falls back to an unreachable worker `InternalIP`;
- no kubelet, metrics, NodePort, host port, LAN listener, or overlay listener
  is added;
- each network-facing process drops to uid/gid 65532 and receives a rotating
  48-hour client certificate for the canonical `kube-system/metrics-server`
  ServiceAccount identity, bound only to stock metrics-server RBAC; and
- `--kubelet-insecure-tls` handles the unavoidable synthetic-loopback SAN
  mismatch, but kubelet application authentication/authorization and the
  authenticated FRP route remain enforced. A stopped tunnel therefore fails
  closed instead of returning synthetic metrics.

Deterministic tests prove the namespace, endpoint, least-privilege identity,
non-root process, and no-publication contracts. The corrected two-host live
gate remains required before hardware parity is claimed.

## Item 8: runtime capability probe

The control plane idempotently owns a `kube-system/dks-runtime-probe`
DaemonSet. It excludes virtual-kubelet nodes, tolerates the
`outpost.dhnt.io/runtime-unavailable` taint so recovery can be observed, and
uses a non-root, read-only, capability-free security context.

The `nodecap` reconciler converts probe state into the cloudbox-compatible
contract:

- `outpost.dhnt.io/runtime-ready=true|false`
- `outpost.dhnt.io/runtime-unavailable=sandbox:NoSchedule`
- `outpost.dhnt.io/runtime-unavailable-reason`

Absence of a probe is unknown rather than failure. A ready probe removes the
taint and reason annotation.

## Required live acceptance

On a control-plane host and a distinct peer worker:

1. Run `script/dks-peer-acceptance.sh` against the peer kubeconfig.
2. Confirm both nodes are Ready with distinct PodCIDRs.
3. Confirm cross-node DNS, service routing, logs, exec, and port-forward.
4. Confirm the runtime probe runs on physical nodes and nodecap state recovers.
5. Confirm `kubectl top nodes` and `kubectl top pods -A` report both hosts.
6. Stop the worker tunnel and confirm logs/exec/metrics fail closed.
7. Reconnect or leave/rejoin and confirm stale-node GC does not delete a live
   replacement.

Until that gate runs on two hosts, hardware parity remains unproven.
