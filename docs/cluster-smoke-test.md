# Cluster smoke test — vkpodman end-to-end against a local k3s

This is the runbook for validating the `internal/agent/vkpodman` package
end-to-end **without involving cloudbox at all.** Outpost talks to any
Kubernetes API server; for the smoke test we stand up a local `k3s
server` and point outpost-vk at it.

If you're auditing the cluster path before the cloudbox-side Tier-1
work lands, this is the test that proves the outpost half is sound.

## Prerequisites

- A **Linux host** (a Linux VM is fine — macOS hosts can't run k3s
  directly, only inside a Linux VM). Validated against k3s
  `v1.31.x-k3s1` on Ubuntu 24.04 and Fedora 41.
- **Podman** ≥ 5.x with the libpod REST socket reachable
  (`podman system service --time=0 unix:///run/user/$UID/podman/podman.sock &`
  on rootless setups; the systemd-managed `podman.socket` user unit
  works the same way).
- **kubectl** ≥ 1.30 in `$PATH`.

## Step 1 — Run k3s

The slimmest configuration that matches the plan (no networking, no
local-storage, no system pods we don't care about):

```bash
sudo curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC=\
  "server --disable=traefik,servicelb,local-storage,metrics-server,coredns \
          --disable-network-policy --disable-kube-proxy \
          --flannel-backend=none \
          --kube-controller-manager-arg=node-monitor-grace-period=40s" \
  sh -
```

Verify:

```bash
sudo k3s kubectl get nodes
# NAME           STATUS   ROLES                  AGE   VERSION
# ip-10-0-...   Ready    control-plane,master   30s   v1.31.1+k3s1
```

The single Ready node here is the k3s host itself (it always runs its
own kubelet on the control-plane node). Our virtual-kubelet will appear
*alongside* it in the next steps.

Copy the kubeconfig somewhere readable:

```bash
sudo cp /etc/rancher/k3s/k3s.yaml ~/k3s-smoke.yaml
sudo chown $USER ~/k3s-smoke.yaml
chmod 600 ~/k3s-smoke.yaml
```

## Step 2 — Build outpost-vk

From the outpost repo:

```bash
go build -o ./bin/outpost-vk ./cmd/outpost-vk
```

## Step 3 — Join the cluster as a virtual node

```bash
./bin/outpost-vk \
  --kubeconfig ~/k3s-smoke.yaml \
  --node smoke-test \
  --podman-socket /run/user/$(id -u)/podman/podman.sock
```

Expected log:

```
INFO vkpodman: reconcile complete containers=0 pods_cached=0
INFO vkpodman: starting node controller node=smoke-test
INFO vkpodman: starting pod controller node=smoke-test
INFO vkpodman: running node=smoke-test podman_socket=/run/user/1000/podman/podman.sock apiserver=https://127.0.0.1:6443
```

In another terminal:

```bash
export KUBECONFIG=~/k3s-smoke.yaml
kubectl get nodes
# NAME           STATUS   ROLES                  AGE   VERSION
# ip-10-0-...   Ready    control-plane,master   2m    v1.31.1+k3s1
# smoke-test    Ready    <none>                 5s    v0.1.0-vkpodman
```

`smoke-test` should flip to `Ready` within ~10 seconds. If it doesn't,
see Troubleshooting below.

## Step 4 — Run a Pod on the virtual node

Apply a Pod with a `nodeSelector` so the scheduler places it on us
specifically. The toleration matches the standard virtual-kubelet
taint vkpodman stamps on its node:

```bash
kubectl apply -f - <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: hello
spec:
  nodeSelector:
    outpost.dhnt.io/host: smoke-test
  tolerations:
    - key: virtual-kubelet.io/provider
      operator: Exists
      effect: NoSchedule
  containers:
    - name: main
      image: docker.io/library/alpine:3.20
      command: ["sh", "-c", "echo hi from $(hostname); sleep 3600"]
YAML
```

Verify it lands on the virtual node and as a podman container:

```bash
kubectl get pod hello -o wide
# NAME   READY  STATUS    RESTARTS  AGE  IP   NODE         ...
# hello  1/1    Running   0         8s   ...  smoke-test   ...

podman ps --filter label=outpost.io/managed=true \
  --format '{{.Names}}\t{{.Image}}\t{{.Status}}'
# outpost-XXXXXXXX-main  docker.io/library/alpine:3.20  Up 6 seconds
```

The container name pattern is `outpost-<first-8-of-pod-uid>-<container-name>`
— see `internal/agent/vkpodman/translate.go`.

## Step 5 — Logs and lifecycle

```bash
kubectl logs hello
# hi from outpost-XXXXXXXX-main

kubectl delete pod hello

podman ps -a --filter label=outpost.io/managed=true
# (empty — container removed as part of DeletePod)
```

## Step 6 — Reconnect reconciliation

Kill outpost-vk while a Pod is running, restart it, observe that the
existing container is *adopted* rather than recreated:

```bash
kubectl apply -f - <<'YAML'
apiVersion: v1
kind: Pod
metadata: {name: persisted}
spec:
  nodeSelector: {outpost.dhnt.io/host: smoke-test}
  tolerations:
    - {key: virtual-kubelet.io/provider, operator: Exists, effect: NoSchedule}
  containers:
    - {name: main, image: alpine, command: ["sleep", "9999"]}
YAML

# wait for the pod to be Running, then Ctrl-C outpost-vk
# and start it again. In the new logs you should see:
#   INFO vkpodman: reconcile complete containers=1 pods_cached=1
#   INFO vkpodman: adopted existing container pod=default/persisted container=...
# and `podman ps` shows the same container ID as before — no duplicate.
```

This validates the reconcile path described in the plan ("adoption is
idempotent — reconcile finds containers by `outpost.io/pod-uid` label
and reuses them").

## Troubleshooting

- **Node stuck NotReady** — outpost-vk can reach the apiserver but the
  apiserver can't reach back. Confirm the `Ping` heartbeat is firing
  (check libpod's logs for `/libpod/_ping` requests) and that
  `kubectl describe node smoke-test` shows recent `LastHeartbeatTime`.
  Lease-based heartbeat is enabled by default via virtual-kubelet's
  `WithNodeEnableLeaseV1` — if your k3s is < 1.20 (it isn't, but just
  in case), the lease API may not exist and the node controller falls
  back to status-only heartbeat which is coarser.

- **Pod stays Pending with `0/1 nodes are available`** — the
  scheduler is filtering out our virtual node. Two common reasons: (1)
  missing the `virtual-kubelet.io/provider` toleration in the spec
  (added at admission by the cloudbox `ValidatingAdmissionPolicy` in
  production, but you have to add it manually in this no-cloudbox
  smoke test); (2) `nodeSelector` mismatch — confirm `kubectl get node
  smoke-test --show-labels` actually has `outpost.dhnt.io/host=smoke-test`.

- **`pull access denied`** — libpod couldn't authenticate to the
  registry. Pre-pull the image with `podman pull <image>` first; or
  configure `~/.config/containers/auth.json`. The translator does not
  pass `imagePullSecrets` (out of scope for v1).

- **"podman socket not detected"** — pass `--podman-socket` explicitly.
  Default-detected paths are in `internal/agent/builtin_apps.go`
  (`podmanCandidates`). On rootless systemd it's normally
  `/run/user/<uid>/podman/podman.sock`.

- **Container with the wrong restart behavior** — Pod spec
  `restartPolicy: Always` maps to libpod `--restart=always`;
  `OnFailure` → `on-failure`; `Never` and unset → no libpod restart
  policy. The cluster controller handles "restart the Pod on a
  different node" separately; this is just the in-container restart.

## How this maps to the production flow

In production, cloudbox issues the kubeconfig instead of you pasting
k3s.yaml — `POST <cloudbox>/api/cluster/kubeconfig` returns the
APIURL + per-host ServiceAccount token + CA. The outpost admin UI
calls that endpoint and persists the same three fields. The cluster
runner code path (`internal/agent/vkpodman/runner.go`) is identical
either way — only the source of the credentials changes.

See the project plan at
`~/.claude/plans/pooling-podman-containers-registered-wit-steady-reef.md`
for the cloudbox-side checklist (k3s systemd unit, two reverse-proxy
routes, the token-issuing endpoint, the RBAC controller, the UserFleet
CRD, the ValidatingAdmissionPolicy). None of that exists in
`/Users/you/projects/poc/cloudbox` yet — that's the next phase.

## Verify cross-node cluster networking

A node being `Ready` only proves that its kubelet can report status; it does not
prove that pods on different nodes share one cluster network. Run the
cross-node checker against a live DKS cluster:

```bash
./scripts/cluster-crossnode-check.sh --kubeconfig ~/cluster.yaml
```

The script pins a short-lived probe to every Ready node. Each probe checks
cluster DNS, the `kubernetes.default` API Service, a pod IP on another node,
and a dedicated ClusterIP Service backed by that remote pod. It also checks
cluster-wide pod IPs and every Endpoints object for duplicate addresses, which
catches overlapping per-node pod ranges and silent local misroutes. NotReady
nodes are reported and skipped; on a one-node cluster the two cross-node checks
are explicitly skipped without failing.

By default, all probe resources are placed in a temporary namespace that is
deleted on exit. To use an existing namespace instead:

```bash
./scripts/cluster-crossnode-check.sh \
  --kubeconfig ~/cluster.yaml \
  --namespace cluster-diagnostics
```

The existing namespace is retained, and the script deletes only Pods and
Services carrying its unique run label. Exit status is zero only when every
applicable check reports `PASS`.
