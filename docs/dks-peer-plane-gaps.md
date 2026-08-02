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
| 7 | Kubelet metrics and `kubectl top` | Closed in code | Pending |
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

k3s's packaged metrics-server remains enabled. The peer control-plane entrypoint
writes a `HelmChartConfig` that makes metrics-server:

- run with host networking so it can reach the control-plane host's loopback
  tunnel listeners;
- prefer `ExternalIP`, then `InternalIP`, then `Hostname`;
- use each node's status kubelet port; and
- tolerate the peer kubelet certificate/address mismatch with
  `--kubelet-insecure-tls`.

The `nodeaddr` reconciler supplies the derived loopback `ExternalIP` and
kubelet port. An unreachable tunnel fails closed; no synthetic metrics are
returned.

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
