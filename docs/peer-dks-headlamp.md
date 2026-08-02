# Headlamp on a peer-hosted DKS plane

`internal/agent/headlamp` deploys and audits a [Headlamp](https://headlamp.dev)
operating UI on a peer-hosted DKS control plane, and
`script/dks-headlamp-verify.sh` proves a deployed instance is running,
answering, **token-gated** (an unauthenticated request yields no cluster
authority), and **not** reachable unauthenticated.

**Why here.** cloudbox's Cluster page is a cross-host *inventory* only — it
structurally cannot operate a peer-hosted plane (its endpoint is not even
registered when cloudbox runs no cluster). Operating a cluster belongs where
the kubeconfig is: the control-plane host. Headlamp is that operating UI —
and it is also a full cluster-admin surface when fed an admin credential, so
the auth boundary below was designed before the deployment shape.

**No cloudbox dependency.** Nothing in the deploy, verify, or operate path
touches cloudbox. Deploy talks to the plane's apiserver via the local
kubeconfig; the operator reaches the UI over a loopback port-forward; remote
reach rides the mesh forwarder. cloudbox remains what it already was for the
mesh (rendezvous/signaler) and is otherwise uninvolved.

## The auth boundary — three layers, none optional

Exposing Headlamp carelessly hands cluster-admin to anyone who can reach the
port. The design removes that possibility at three independent layers; a
single mistake in any one of them does not open the cluster.

### 1. The pod holds no power (token-login model)

The Headlamp pod's ServiceAccount (`headlamp/headlamp`) is bound to **zero**
RBAC grants — no ClusterRoleBinding, no RoleBinding, nothing. Headlamp runs
in token-login mode: every apiserver request is authenticated with the token
the operator pastes into the UI for that session. Consequences:

- A compromised Headlamp pod (or a vulnerability in Headlamp itself) yields
  only an unprivileged SA token (`system:basic-user` discovery), never
  cluster-admin.
- Reaching the UI's login screen grants nothing. Authority enters the system
  only when an operator pastes a token, and leaves with the session.
- This package never mints, stores, or persists an admin credential. Admin
  access is always the operator pasting a token they already hold.

The SA token automount stays on solely so Headlamp's in-cluster config
(apiserver address + CA) resolves; the token itself is powerless.

### 2. No network path without authentication

The Service is **ClusterIP only** — never NodePort, never LoadBalancer, no
externalIPs, no hostNetwork/hostPort anywhere in the pod spec. The only way
off the node is a port-forward pinned to `127.0.0.1`
(`headlamp.ForwardArgs`), which follows outpost's standing rule: the local
surface binds loopback, and the tunnel/mesh is the only remote ingress.

Remote reach uses the **existing authenticated surfaces**, never a new port:

- **Mesh forwarder (primary).** On the control-plane host:
  `outpost mesh service add headlamp 127.0.0.1:18466` (after starting the
  forward, e.g. `$(outpost cluster kubeconfig >/dev/null && kubectl -n
  headlamp port-forward --address 127.0.0.1 svc/headlamp 18466:80)` — or the
  exact argv from `headlamp.ForwardArgs`). From the operator's workstation:
  `outpost mesh dial headlamp` (or `mesh listen <peer-id> headlamp`), then
  browse the returned local address. The forwarder reaches **allowlisted
  service names only** — a connected peer can never dial an arbitrary local
  port — and that allowlist is the remote auth gate. Peer identity is the
  paired mesh key; cross-owner peers are isolated.
- **Matrix-tunnel app (alternative, for portal users).** Register the
  loopback forward as an app with `RequireLogin: true`; the per-(host, app)
  `matrix_elev` elevate flow then gates every cloud-side request. This path
  involves cloudbox by definition — use it only when portal-surface access
  is actually wanted.

No LAN bind is offered anywhere in this package, and
`headlamp.ValidateListenAddr` rejects non-loopback binds with no override
parameter. If a follow-up ever offers a LAN bind, it must force an auth gate
on every request and warn — the existing `admin_addr` precedent — as a
conscious, separate change.

### 3. RBAC scope of the optional viewer (and nothing else)

The one credential this package can create is the **optional** read-only
viewer ServiceAccount `headlamp/headlamp-viewer` (no pod mounts it; it
exists so the operator can mint a scoped audit token to paste at login
instead of an admin one). Its exact grants, and why:

| Grant | Scope | Why |
|---|---|---|
| built-in aggregate ClusterRole `view` | namespaced reads; **excludes Secrets and RBAC objects** | the standard least-privilege read role; upstream-maintained coverage of new resource types |
| `outpost-headlamp-nodeview` (package-owned) | `nodes`: get/list/watch **only** | nodes are cluster-scoped, so `view` alone cannot show them, and a control-plane operating UI that cannot list nodes is pointless |

Nothing else. No write verb, no Secrets, no RBAC reads, no
escalate/bind/impersonate. Set `Options.DisableViewerRBAC` to converge the
viewer SA, its ClusterRole, and both bindings away (`Deploy` removes them;
`RenderJSON` omits them).

**A minted viewer token is a cluster credential — owner-only.** Read-only,
but cluster-wide, and the tenancy model is absolute: non-owners get no
cluster credential; guests reach shared apps via Periscope. A minted viewer
token must therefore **never reach a guest or non-owner** — never pasted
into a Headlamp session anyone but the owner can drive, never handed out,
never persisted. (The SA exists by default because creating it confers no
credential by itself: minting requires cluster-admin, an owner-only act.
`DisableViewerRBAC` removes even the minting target.)

Mint a viewer token when needed (short-lived, never persisted):

```bash
kubectl -n headlamp create token headlamp-viewer --duration=1h
```

### Drift detection

`headlamp.Inspect` audits the live plane against all three layers and
reports violations: a Service flipped to NodePort/LoadBalancer or grown
externalIPs, hostNetwork/hostPID/hostIPC/hostPort/privileged anywhere in the
pods, a pod running as any SA other than the zero-grant one, **any** binding
referencing the pod SA, or any binding beyond the two package-owned ones
referencing the viewer SA. `Inspect` is fail-closed at the API layer: an
unanswerable query is an error, never a clean posture. Re-running
`headlamp.Deploy` repairs the drift it can (Service type, widened nodeview
rules, rewritten binding roleRefs).

## Deploying

Hosts are named by role: the **control-plane host** runs the DKS control
plane and holds the kubeconfig; the **operator workstation** is any paired
peer the operator browses from; **workers** are the other nodes.

On the control-plane host, either call `headlamp.Deploy(ctx, clientset,
headlamp.Options{})` from Go, or render and apply:

```bash
# Render the manifest (v1 List, JSON — kubectl applies it like YAML):
#   headlamp.RenderJSON(headlamp.Options{})  →  headlamp.json
kubectl --kubeconfig ~/.kube/outpost-control-plane/k3s.yaml apply -f headlamp.json
```

Defaults: namespace `headlamp`, image `ghcr.io/headlamp-k8s/headlamp:v0.27.0`
(a pin, not a floating tag), Service port 80 → container 4466, loopback
forward port 18466. The pod runs non-root, no privilege escalation, all
capabilities dropped, RuntimeDefault seccomp, with resource requests/limits.
`Remove` deletes everything, but only deletes the namespace when it carries
the `app.kubernetes.io/managed-by=outpost` label — a namespace this package
did not create is never destroyed (and `Deploy` refuses to adopt one).

## Verifying — `script/dks-headlamp-verify.sh`

Run from any host with LAN/tailnet reach to the plane's nodes (the
control-plane host always qualifies):

```bash
./script/dks-headlamp-verify.sh
```

(The script uses `~/.kube/outpost-control-plane/k3s.yaml` — the peer admin
kubeconfig — deterministically, canonicalizing via `readlink -f`. An ambient
`KUBECONFIG` env var is rejected as a FAIL rather than silently selecting the
wrong cluster.)

Six checks, one `CHECK <name> PASS|FAIL|BLOCKED|NOT-RUN <detail>` line each
(same exit contract as `script/dks-peer-acceptance.sh`, whose check 9 probes
Headlamp *inside* the cluster and selects on the same
`app.kubernetes.io/name=headlamp` label this package stamps):

1. `running` — the deployment has ≥1 ready replica.
2. `service-clusterip-only` — type ClusterIP, no nodePorts, no externalIPs.
3. `rbac-zero-grant` — no binding anywhere references the pod SA.
4. `answering` — a loopback-pinned port-forward yields an HTTP status line.
   (What that answer is *worth* is the next check's job, not this one's.)
5. `auth-token-required` — the positive auth assertion: an **unauthenticated**
   GET of the apiserver-proxy route (`/clusters/main/api/v1` — the pinned
   image v0.27.0 names its in-cluster context `main`, and the in-cluster
   context carries no credentials of its own) must be denied: 401 or 403 is
   the gate working. A 2xx is authority without a token — **FAIL**. Any other
   answer means the pinned route layout drifted and the gate is UNCONFIRMED —
   BLOCKED, never a silent pass and never a false fail.
6. `no-unauthenticated-exposure` — the negative check: **every discovered
   node address of every type** (InternalIP, ExternalIP — the single most
   likely place a Headlamp would actually be exposed — and Hostname) is
   probed on the Headlamp container port plus any drifted nodePort, and each
   must be **actively refused on a host proven live**.

**Exit contract — disclosed in full.** Exit 0 (OK) means *every required
check ran and passed*. Exit 1 (FAIL) means at least one check failed. Exit 2
(INCONCLUSIVE) means the verdict cannot be called OK **or** clean-FAIL: any
check BLOCKED (a blocked security assurance is not OK), or any required
check **never ran** (NOT-RUN — `HLV_ONLY` exists for focused debugging and a
subset run closes every unselected check NOT-RUN, so CI invoking a subset
can never report green forever). No success state is reachable by the
absence of evidence.

**Evidence invariant — the negative check fails closed.** A refusal counts
as "not exposed" only after a control port on the same address (default
10250, the kubelet) answered, proving the prober works and the address is a
live host. Connection-refused for the wrong reason — wrong host, dead host,
DNS failure, broken prober — is BLOCKED, never PASS. A timeout or any
unclassifiable outcome is BLOCKED. An observed open port is FAIL even when
the control could not be validated. The **address set is held to the same
standard as the port set**: an unreadable Service (unknown nodePorts)
BLOCKs, and so does a node with no address, and so does an `HLV_PROBE_ADDRS`
override that leaves any discovered node uncovered — partial evidence is
never scored as "correctly not exposed".

`script/dks-headlamp-verify_test.sh` proves the scoring offline (no cluster,
no kubectl, no network): unit tests of the pure helpers plus behavioral runs
of the real harness against a stub kubectl. That covers every per-check
scoring decision **and the aggregate verdict rules**, including the
false-green scenarios: refusal on an unproven host, timeouts, probe crashes,
an unreadable Service hiding a nodePort, an ExternalIP-only exposure, a
subset (`HLV_ONLY`) run that must never be green, a narrowing
`HLV_PROBE_ADDRS` that must never score an unqualified PASS, and an
unauthenticated 2xx that must FAIL. What remains unproven is only the live
transport it all rides on — see the evidence boundary below.

## Four-surface wiring — `headlamp_enabled`

Headlamp is wired as a builtin following the loom/zot pattern on all four
surfaces:

- **File** (`agent.json`): `headlamp_enabled` (`*bool`, default off) and
  `headlamp_port` (default `headlamp.DefaultLocalPort`, validated through
  `headlamp.ValidateListenAddr` composition — loopback only).
- **CLI**: `outpost builtins set --headlamp=on --headlamp-port N`.
- **MCP**: `outpost_set_builtins` with `headlamp` / `headlamp_port` args.
- **UI**: `SafeView.HeadlampEnabled` / `HeadlampPort` fields, toggled
  through the admin UI's Built-ins page.

On boot when enabled and the cluster control plane is on: run
`headlamp.Deploy` against the plane, supervise the loopback port-forward
(`ForwardArgs`), and auto-expose it over the mesh as `headlamp` — the same
supervise-and-expose shape `startBashyServiceSupervisors` gives loom. A
change triggers a **restart** (the deployment + forward are wired at boot).

## Evidence boundary — what is and is not proven

**Proven (deterministic, no cluster):** manifest shape and its security
posture (`go test ./internal/agent/headlamp/...` — zero-grant pod SA,
ClusterIP-only, viewer scope, drift repair, fail-closed Inspect, all against
`client-go`'s fake clientset), and every verify-harness scoring decision
(`bash script/dks-headlamp-verify_test.sh`).

**HARDWARE-UNPROVEN:** Headlamp against a real peer-hosted plane. The image
pin (v0.27.0), token login against a live DKS apiserver, the in-cluster
config resolution with a powerless SA, the `/clusters/main/...` proxy route
the auth-token-required assertion targets (verified against the pinned
image's source, not against a live server), the port-forward under the real
CNI, and the mesh-forwarded browse from a peer workstation have **not** been
exercised on hardware. First hardware pass: deploy on the control-plane
host, run `script/dks-headlamp-verify.sh` there, then
`script/dks-peer-acceptance.sh` (check 9), then a viewer-token login from an
operator workstation over `outpost mesh dial headlamp` — and record the
results before trusting this surface operationally.

## Live two-node evidence procedure — UNPROVEN

**DO NOT RUN THIS YET and DO NOT CLAIM ANY HARDWARE RESULT.** This section
defines the procedure for proving cross-node Headlamp operation on a real
two-node cluster. Record results below when the procedure is executed.

### Prerequisites

- Two outpost hosts, both paired with the same cloudbox.
- Host A: the **control-plane host** — running `cluster.control_plane` with
  the kubeconfig at `~/.kube/outpost-control-plane/k3s.yaml`.
- Host B: a **worker** — joined to Host A's plane via `outpost cluster join`.
  Must run a real k3s-agent node (not vknode) because `kubectl logs` and
  `kubectl exec` both require a real kubelet on the target node.
- Both hosts must be on the same LAN or have mesh reachability.
- Headlamp deployed on Host A (via `headlamp.Deploy` or the four-surface
  toggle with `headlamp_enabled: true`).

### Pass criterion

Each numbered test below specifies its explicit pass criterion. A test is
PASS only when the criterion is met exactly; "looks right" or "seems to work"
is not a pass.

### Test 1: Service DNS resolution across nodes

1. From Host A, start the loopback port-forward:
   ```
   kubectl --kubeconfig ~/.kube/outpost-control-plane/k3s.yaml \
     -n headlamp port-forward --address 127.0.0.1 svc/headlamp 18466:80
   ```
2. From Host B, resolve the Headlamp Service via the cluster DNS:
   ```
   kubectl --kubeconfig ~/.kube/outpost-control-plane/k3s.yaml \
     run dns-test --rm -it --restart=Never --image=busybox:1.36.1 \
     -- nslookup headlamp.headlamp.svc.cluster.local
   ```
   **Pass criterion:** `nslookup` returns a ClusterIP address (10.x.x.x)
   for `headlamp.headlamp.svc.cluster.local`. A timeout, NXDOMAIN, or
   resolution failure is a FAIL.

### Test 2: Cross-node `kubectl logs`

On Host A (the control-plane host):

```
kubectl --kubeconfig ~/.kube/outpost-control-plane/k3s.yaml \
  logs -n headlamp deployment/headlamp
```

**Pass criterion:** Logs stream from the Headlamp pod, regardless of which
node the pod is scheduled on. A logs call that fails, times out, or returns
empty when the pod is running is a FAIL.

NOTE: `kubectl logs` requires a real kubelet on the target node — it is NOT
implemented for vknode pods. The worker (Host B) must therefore run a real
k3s-agent, not a virtual-kubelet node, for this test to produce evidence
(run `outpost builtins set --cluster-agent=on` on Host B).

### Test 3: Cross-node `kubectl exec`

On Host A:

```
kubectl --kubeconfig ~/.kube/outpost-control-plane/k3s.yaml \
  exec -n headlamp deployment/headlamp -- wget -qO- http://localhost:4466/healthz
```

**Pass criterion:** The HTTP response from Headlamp's `/healthz` endpoint
returns a status line (200 or another 2xx/3xx). A connection-refused, timeout,
or empty body is a FAIL.

### Test 4: Cleanup

After all tests:

```
kubectl --kubeconfig ~/.kube/outpost-control-plane/k3s.yaml \
  delete pod dns-test -n default 2>/dev/null
```

**Pass criterion:** No temp pods, logged credentials, or port-forward
processes remain on either host.

### Record

Fill in when the procedure is run:

| Test | Pass/Fail | Date | Notes |
|---|---|---|---|
| 1. DNS resolution | — | — | — |
| 2. Cross-node logs | — | — | — |
| 3. Cross-node exec | — | — | — |
| 4. Cleanup | — | — | — |
