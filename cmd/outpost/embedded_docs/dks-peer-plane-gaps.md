# DKS Peer-Hosted Control Plane Gap Tracker

This document tracks known implementation gaps and closure status for peer-hosted DKS control planes (`outpost cluster control-plane`).

---

## Gap Inventory

### 1. Control Plane Listener and Tunnel Supervision [CLOSED]
Hosted peer planes supervise frps beside k3s to establish worker tunnels on loopback without requiring cloudbox.

### 2. Node Address Resolution (`nodeaddr`) [CLOSED]
Derived per-node kubelet ports ensure `kubectl logs`, `exec`, and `port-forward` dial through the tunnel namespace correctly.

### 3. Stale Node Garbage Collection (`nodegc`) [CLOSED]
Bounded GC cleans up NotReady node ghosts after the 24 h grace period on peer worker departure/rejoin.

### 4. Peer Pod Overlay Networking (Flannel) [CLOSED]
Stock k3s flannel VXLAN pinned to the Tailscale interface (`--flannel-iface=tailscale0`) for cross-node pod routing without cloudbox.

### 5. Peer Image Distribution [CLOSED]
Node-local OCI caching and peer registry distribution path for workloads without cloudbox image proxy.

### 6. Runtime Sandbox Probe DaemonSet Deployment (`dks-runtime-probe`) [CLOSED]

#### Problem
The `nodecap` reconciler existed and was started for hosted peer planes to translate container runtime readiness into truthful node scheduler state (`outpost.dhnt.io/runtime-ready` label and `outpost.dhnt.io/runtime-unavailable` taint). However, its backing `dks-runtime-probe` DaemonSet was not deployed to `kube-system` by the control plane supervisor, leaving `nodecap` with no probe pods to observe.

#### Solution
Implemented an idempotent peer-plane-owned probe deployment manager in `internal/agent/nodecap`.

- **Deployment & Namespace**: Manages `DaemonSet` named `dks-runtime-probe` in `kube-system`.
- **Exact Selector**: Uses `app.kubernetes.io/name=dks-runtime-probe`.
- **Image Choice**: Defaults to `rancher/mirrored-pause:3.6` (guaranteed by k3s containerd; no cloudbox dependency).
- **Node Selection & Exclusion**: Excludes virtual-kubelet nodes (`outpost.dhnt.io/runtime=virtual`) using NodeAffinity (`outpost.dhnt.io/runtime` `NotIn` `["virtual"]`).
- **Tolerations**: Explicitly tolerates `outpost.dhnt.io/runtime-unavailable` (Effect: `NoSchedule`), allowing the probe pod to remain running on tainted nodes to demonstrate runtime recovery. Also tolerates standard master and control-plane taints.
- **Least Privilege**: SecurityContext drops all capabilities (`Capabilities.Drop = ["ALL"]`), enforces `RunAsNonRoot: true`, `RunAsUser: 65534` (`nobody`), `AllowPrivilegeEscalation: false`, and `ReadOnlyRootFilesystem: true`. Resource requests/limits are set to minimal bounds (1m/4Mi request, 10m/16Mi limit).
- **Bounded Rollout**: Configured with `RollingUpdate` strategy (`MaxUnavailable: 25%`).
- **Cleanup & Disable**: `DeleteProbeDaemonSet` / `r.DisableProbeDaemonSet = true` safely removes the probe DaemonSet from `kube-system`.
- **Vocabulary Parity**: Identical label, taint, and annotation keys as cloudbox-hosted planes:
  - Taint: `outpost.dhnt.io/runtime-unavailable` (Value: `sandbox`, Effect: `NoSchedule`)
  - Ready Label: `outpost.dhnt.io/runtime-ready` (`true`/`false`)
  - Runtime Label: `outpost.dhnt.io/runtime` (`virtual`)
  - Reason Annotation: `outpost.dhnt.io/runtime-unavailable-reason`

#### Verification & Test Matrix
1. **Absent Probe**: Absence of probe pod leaves node untouched (status unknown, not failure).
2. **Failed Sandbox Taint**: Probe PodNotReady past `ProbeGrace` (2 min) taints node `outpost.dhnt.io/runtime-unavailable=sandbox:NoSchedule`, sets `outpost.dhnt.io/runtime-ready=false`, and adds reason annotation.
3. **Recovery**: Probe PodReady=True removes taint and reason annotation, sets `outpost.dhnt.io/runtime-ready=true`.
4. **Rollout Duplicates**: Handles multi-probe pods during rolling updates (prefers Ready probe; falls back to oldest unready probe).
5. **Vocabulary Parity**: Verified keys and selectors match cloudbox specification.

#### Live Two-Node Proof
- Status: **UNPROVEN**
- Note: Live hardware execution requires a multi-node peer-hosted control plane cluster with active worker nodes. Unit and fake-clientset tests in `internal/agent/nodecap` verify contract adherence.
