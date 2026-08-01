# Peer-hosted DKS control plane

This runbook covers hosting a single DKS control plane on one outpost and
joining another outpost to it. The machines are named by role throughout:
the **control-plane host** runs the apiserver, and the **tunnelled worker**
joins it through the outpost tunnel.

## 1. Concepts

Hosting and joining are independent choices. An outpost can host a control
plane for other machines while joining a different plane itself. Neither
choice changes cloudbox pairing: paired apps, shell access, the LLM pool, and
fleet registration continue to use cloudbox.

Control-plane placement is a user choice. It can be cloudbox-hosted,
peer-hosted, or external; moving the apiserver is a configuration decision,
not a different DKS architecture.

A cluster's stable identity is the apiserver CA fingerprint, not its URL.
Every member trusts the same CA, while tunnelled workers reach that plane at
their own loopback URL and may use different local ports. The CA hash embedded
in a k3s node token provides that identity when the CA bundle is not stored on
the worker.

## 2. Host a control plane

Enable hosting on the **control-plane host** through any one of the four
settings surfaces:

| Surface | Setting or action |
|---|---|
| Config file | Set `cluster.control_plane` to `true` in `agent.json`, then restart outpost |
| CLI | `outpost cluster control-plane on` |
| Admin UI | Inbound > Cluster > Host the apiserver |
| MCP | Call `outpost_set_control_plane` with `enabled: true` |

The CLI and other live settings surfaces schedule an outpost restart when one
is needed. The tunnel defaults to `127.0.0.1:7000`, for access over the mesh.
To accept tunnel connections directly from the network, the CLI supports
`outpost cluster control-plane on --bind-addr 0.0.0.0`; expose that listener
only on a network you intend workers to use. `--bind-port` changes its port.

At boot, outpost starts a privileged k3s server container and runs frps beside
k3s in that same container and network namespace. The hosted apiserver is
published on `127.0.0.1:16443`. The admin kubeconfig is written on the host at:

```text
~/.kube/outpost-control-plane/k3s.yaml
```

Outpost records that path as `cluster.control_plane_kubeconfig` so its
cluster-wide reconcilers and the operator can use the hosted plane.

The daemon supervises the control-plane container for its lifetime. It starts
an existing stopped container, retries failed bring-up with backoff, probes
the apiserver rather than trusting container state alone, and recreates a
container that remains unhealthy past the grace period. On normal daemon
shutdown it removes the container but preserves the k3s data volume, so an
ordinary restart does not destroy cluster identity or invalidate node tokens.

Check the hosted plane with:

```bash
outpost cluster control-plane status
```

The command reports whether hosting is configured, container existence and
running state, whether the apiserver is serving, the join endpoint, the node
list and each node's readiness, and `node_count`. Credentials are never
revealed: `has_join_token`, `has_node_token`, and `has_stcp_secret` are
presence booleans only.

## 3. Retrieve the join credentials

A tunnelled worker needs the endpoint and three credentials. Retrieve them on
the **control-plane host** only:

```bash
outpost cluster control-plane token
outpost cluster token
```

`outpost cluster control-plane token` prints the endpoint, the hosted plane's
TunnelToken, and its STCP secret. The TunnelToken authenticates the worker's
tunnel session. It is not `cluster.join_token`: that is the worker-side field
where this value is stored when the worker joins somebody else's plane.

`outpost cluster token` prints the separate k3s node token minted inside the
hosted control-plane container. Do not substitute one token for the other.

All three values are credentials. Never paste them into shared documentation,
tickets, chat transcripts, or logs. Prefer
`outpost cluster control-plane token --quiet`, the worker's credential
environment variables, or `--token-stdin` in automation so secret values do
not enter shell history or process listings.

## 4. Join a worker

On the **tunnelled worker**, provide the endpoint from the control-plane host
and all three credentials:

```bash
outpost cluster join <control-plane-host>:7000 \
  --token '<tunnel-token>' \
  --stcp-secret '<stcp-secret>' \
  --node-token '<k3s-node-token>'
```

The endpoint is a `host[:port]`, not an `https://` URL. The command persists
`cluster.join_endpoint`, `cluster.join_token`, `cluster.stcp_secret`, and
`cluster.node_token` in `agent.json`; it enables cluster mode, selects the
`agent` runtime only when no runtime was already selected, clears stale
cloudbox overlay credentials, and restarts a running outpost to apply the
join. The tunnel visitor binds the joined apiserver locally on port `6443` by
default. `outpost cluster join --show` reports the selected plane and
credential presence without revealing values, plus the runtimes selected.

### Selecting runtimes on the joined plane

By default the join registers one real k3s-agent Node. Pass `--cluster-agent`
/ `--cluster-virtual` to register virtual-kubelet Nodes instead of, or
alongside, it:

```bash
# agent + a vk-podman Node
outpost cluster join <control-plane-host>:7000 --token '<tunnel-token>' \
  --cluster-virtual vk-podman

# vk only — no k3s agent on this worker
outpost cluster join --cluster-agent=off --cluster-virtual vk-podman,vk-native
```

A peer-hosted plane supports vk Nodes as-is: the vk kubeconfig loader accepts
the client-certificate credentials k3s issues, as distinct from a
cloudbox-minted bearer token. So this is runtime *selection*, not new runtime
support — before these flags existed, reaching a vk Node on a peer plane meant
hand-editing `agent.json`. It is what lets `VENUE=vk-podman` and vk-native
workloads run on a peer plane.

The two flags spell `cluster.runtimes.agent` and `cluster.runtimes.virtual`,
the same fields `outpost builtins set --cluster-agent/--cluster-virtual`
writes, and follow the same rules:

- an omitted flag leaves the persisted value alone, so a re-join with a
  rotated credential never disturbs the selection;
- `--cluster-virtual` replaces the complete set (`vk-podman`, `vk-native`,
  `vk-ollama`);
- naming **neither** flag keeps the historical default — the `agent` runtime,
  and only when no runtime was already selected, so a join never overwrites a
  selection you made;
- deselecting every runtime at once is refused, rather than quietly
  reinstating the agent runtime under the selection you just made.

The same two arguments are `cluster_agent` / `cluster_virtual` on the
`outpost_cluster_join_peer` MCP tool; every surface routes through
`admincore.JoinPeerPlane`.

After the worker restarts, verify membership from the **control-plane host**:

```bash
kubectl --kubeconfig ~/.kube/outpost-control-plane/k3s.yaml get nodes
```

Wait for the tunnelled worker's node to report `Ready`. The same readiness is
available in `outpost cluster control-plane status`.

## 5. Recover

### The control-plane container exited

Keep the outpost daemon running. Its supervisor restarts an exited matching
container and recreates one whose apiserver stays unhealthy. Use
`outpost cluster control-plane status` to distinguish a missing container, a
stopped container, and a running container whose apiserver is not serving.
Check the outpost and container logs if repeated retries do not recover it;
do not delete the k3s data volume as a routine restart step.

### The apiserver port is already in use

A hosted apiserver must **not** use port `6443`. That port belongs to the STCP
visitor for the plane this host joins, because hosting and joining can happen
on the same machine. The hosted plane uses `16443`. If `16443` is occupied,
stop or reconfigure the conflicting listener; do not move the hosted
apiserver onto `6443`, where kubectl can reach the wrong cluster and report a
misleading CA error.

### Workers have stale node tokens

Recreating the plane's k3s state creates a new CA identity and k3s node token.
The old workers cannot authenticate to the new plane. On the control-plane
host, retrieve the current credentials again with
`outpost cluster control-plane token` and `outpost cluster token`, then rerun
the full `outpost cluster join <endpoint> ...` command on every tunnelled
worker. Confirm the new nodes with `kubectl get nodes` through the hosted
plane's admin kubeconfig.

### The tunnel token was rotated

`outpost cluster control-plane token rotate` (or the admin UI's Rotate
button, or the `outpost_rotate_control_plane_token` MCP tool) mints a new
tunnel token and never touches the STCP secret or the k3s node token — those
are separate credentials with separate owners (see Concepts), and rotation
revokes only the one that leaked. Every worker's frpc session fails its next
reconnect until it is given the new token, but recovery does not require
resending all three credentials.

Rotate on the control-plane host:

```bash
outpost cluster control-plane token rotate --yes
```

The command's own output (and the `worker_rejoin_hint` field on the MCP/API
response) states the recovery command; run it once per worker, piping the
token straight across so it never lands in shell history or a process list
on either machine:

```bash
outpost cluster control-plane token --quiet | ssh <worker> outpost cluster join --token-stdin
```

`outpost cluster join --token-stdin` with no endpoint argument updates only
`cluster.join_token` on the worker — `JoinPeerPlane` treats every other field
as "leave alone", so the worker's endpoint, STCP secret, and node token stay
exactly as already configured. This is a single-field update, not a rejoin:
run it directly on the worker, or from the control-plane host over SSH as
shown above. If a worker is unreachable, its node stays stranded (frpc
retries and fails) until someone runs this on it — rotation itself does not
leave a silent gap, but an offline worker still needs this step once it comes
back.

Confirm recovery the same way as any other join, from the **control-plane
host**:

```bash
kubectl --kubeconfig ~/.kube/outpost-control-plane/k3s.yaml get nodes
```

## 6. Limits

- Multi-node pod networking is implemented as stock k3s flannel VXLAN on
  workers over the Tailscale node underlay
  (`--flannel-iface=tailscale0`). The peer k3s controller-manager is the
  sole pod-CIDR allocator; no cloudbox-style peer allocator is required.
  Cross-node pod, Service, and DNS traffic still needs the two-machine
  hardware acceptance proof before this is considered production-ready.
- `kubectl logs` and `kubectl exec` through a tunnelled worker are not yet
  proven (F19).
- Peer-hosted DKS supports one control plane only. There is no HA control
  plane or backup/restore runbook (G21).
- A Windows tunnelled worker is untested (G20).
