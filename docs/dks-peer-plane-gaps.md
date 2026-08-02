# Peer-Hosted DKS Control Plane Gaps

Audit and closure tracking for peer-hosted DKS (Distributed Kubernetes Service) control plane functionality.

---

## Gap Inventory & Status

| Item | Component | Description | Status |
| --- | --- | --- | --- |
| 1 | Control Plane Boot | Peer-hosted control plane startup via `server-entrypoint.sh` | Closed |
| 2 | Tunnel & Visitor | Worker frpc to control-plane frps STCP visitor setup | Closed |
| 3 | Node Address Patching | apiserver→kubelet addressing via `nodeaddr` (ExternalIP + derived port) | Closed |
| 4 | Stale Node GC | Garbage collection of left/stale worker nodes via `nodegc` | Closed |
| 5 | Peer Join Runtime Selection | Runtime selection (`agent`, `virtual-kubelet`) on `outpost cluster join` | Closed |
| 6 | Pod Network (peer-flannel) | Flannel VXLAN over Tailscale underlay (`--flannel-iface=tailscale0`) | Closed |
| 7 | Kubelet Metrics & `kubectl top` | `kubectl top nodes` / `kubectl top pods` across tunnelled workers | **CLOSED** |

---

## Item 7 Audit & Implementation Details

### Audit Findings

1. **`metrics-server` Presence**:
   - Inspected `internal/agent/runtime/image/server-entrypoint.sh` and k3s default packaged manifests.
   - `k3s server` is executed with `--disable=traefik,servicelb`.
   - `--disable=metrics-server` is **not** specified.
   - k3s includes `metrics-server` as a built-in packaged manifest/addon in `kube-system`.
   - **Conclusion**: `metrics-server` is already enabled by k3s by default in `server-entrypoint.sh`. No duplicate `metrics-server` manifest or deployment was added.

2. **Missing Production Wiring**:
   - Standard k3s `metrics-server` defaults to `--kubelet-preferred-address-types=InternalIP,ExternalIP,Hostname`.
   - For tunnelled worker nodes, `InternalIP` is a container-local or private LAN address that is unreachable across the tunnel from the control-plane container.
   - `nodeaddr.Reconciler` patches nodes with `ExternalIP = 127.0.x.y` and `DaemonEndpoints.KubeletEndpoint.Port = <derived-port>`.
   - To make `metrics-server` scrape metrics across tunnelled workers, `metrics-server` must prefer `ExternalIP` over `InternalIP` (`--kubelet-preferred-address-types=ExternalIP,InternalIP,Hostname`), respect node status ports (`--kubelet-use-node-status-port`), and run with `hostNetwork: true` so the pod can reach the `127.0.x.y` loopback listeners on the host network namespace.

3. **Production Wiring Implemented**:
   - `server-entrypoint.sh` configures k3s's built-in `metrics-server` addon by writing `/var/lib/rancher/k3s/server/manifests/metrics-server-config.yaml` (`HelmChartConfig`):
     - `hostNetwork.enabled: true`
     - `--kubelet-preferred-address-types=ExternalIP,InternalIP,Hostname`
     - `--kubelet-use-node-status-port`
     - `--kubelet-insecure-tls`
   - Operates entirely within the self-hosted peer control plane. No dependency on cloudbox execution.
   - **Fail Closed**: Unavailable or unmapped kubelet metrics endpoints fail closed (return unreachable/error state) rather than fabricating dummy metrics.
   - **Placement Parity**: Peer workers and cloud workers use identical `nodeaddr` address/port derivation contracts.
   - **Deterministic Diagnostics**: `nodeaddr.Reconciler.Diagnostics` exposes deterministic node addressing and patching status without exposing secrets.

---

## Two-Host Live Acceptance Procedure

**Headline: Peer-Hosted DKS Kubelet Metrics (`kubectl top nodes/pods`) across hardware is HARDWARE-UNPROVEN until this live acceptance procedure is executed.**

### Pre-requisites
- **Host A (Control Plane Host)**: Running `outpost cluster control-plane on` with `k3s server` container active.
- **Host B (Worker Host)**: Joined via `outpost cluster join-peer <host-A-address> --token <token>`.
- Both hosts active in the peer control plane; worker B tunnel established to control plane frps listener.

### Procedure

1. **Verify `metrics-server` APIService & Deployment**:
   ```bash
   kubectl get deployment -n kube-system metrics-server
   kubectl get apiservice v1beta1.metrics.k8s.io
   ```
   *Expected*: `metrics-server` deployment has 1/1 replicas Ready, and `v1beta1.metrics.k8s.io` APIService reports `Available = True`.

2. **Verify Node Addressing & Kubelet Endpoint Patch**:
   ```bash
   kubectl get node worker-b -o jsonpath='{.status.addresses}'
   kubectl get node worker-b -o jsonpath='{.status.daemonEndpoints.kubeletEndpoint}'
   ```
   *Expected*: `status.addresses` contains `ExternalIP` set to `127.0.x.y`, and `kubeletEndpoint.port` matches `nodeaddr.KubeletPortForNode("worker-b")`.

3. **Verify `kubectl top nodes` Across Tunnelled Worker**:
   ```bash
   kubectl top nodes
   ```
   *Expected*: Outputs CPU (cores/%) and Memory (bytes/%) for both host A (control plane) and host B (tunnelled worker).

4. **Verify `kubectl top pods` Across All Namespaces**:
   ```bash
   kubectl top pods -A
   ```
   *Expected*: Outputs pod CPU and memory usage for pods running on worker-b without timeout or connection refused errors.

5. **Verify Fail-Closed Behavior on Unavailable Metrics**:
   - Temporarily stop `frpc` on worker B host.
   - Run `kubectl top node worker-b`.
   - *Expected*: Command fails closed with connection error/timeout. No dummy or stale metrics are returned.
