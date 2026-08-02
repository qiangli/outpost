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

A worker joining with the `agent` runtime must also be **paired with
cloudbox**: its pod network is stock flannel VXLAN over the Tailscale
underlay, and the tailnet coordinator (Headscale) lives on cloudbox. At boot
the daemon fetches a fresh single-use tailnet auth key over the pairing's
access token, and the runtime container registers through a dedicated
overlay-relay tunnel session to cloudbox authorized by the automatically
captured `cluster.cloud_stcp_secret`. Nothing is typed for any of this; if
the daemon logs that the relay secret is missing (`has_cloud_stcp_secret`
false in `outpost status`), pair the host — or restart it once while
cloudbox is reachable — and the secret is captured on the next boot
reattach. The agent refuses to start rather than joining as a node with no
tailnet identity, and the runtime container is supervised, so a worker that
failed to come up during a cloudbox outage recovers on its own once the
outage ends.

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
workloads run on a peer plane, and the real k3s agent and any selected virtual
backends register concurrently as independent Nodes owned by this host.

#### vk credentials and namespace policy on a peer plane

The persistent daemon runs the peer vk path **without any cloudbox call**. It
does not fetch a kubeconfig or a namespace allow-set from cloudbox and does not
start the cloudbox token/access refreshers. Instead it derives everything
locally:

- **Peer CA identity.** The node trusts `cluster.ca` (the peer plane's CA
  bundle, PEM) — not cloudbox's CA and not the system roots. Supply it in
  `agent.json` when joining a peer plane whose apiserver is self-signed (the
  k3s default).
- **Local credential.** The vk node authenticates with a locally held
  credential: `cluster.client_cert` + `cluster.client_key` (the client-cert
  form k3s issues, which wins when present) or `cluster.token` (a k3s bearer
  credential). It is materialized to `~/.cache/outpost/cluster-peer/` at mode
  `0600`. A bearer token is written as a file so client-go re-reads a rotation
  live; rotating it (save a new `cluster.token`) is picked up within a minute
  with no cloudbox involvement. A client-cert rotation takes effect on the next
  restart.
- **Apiserver address.** The node dials `https://127.0.0.1:<k8s_api_port>`
  (default 6443) — the same loopback visitor the k3s agent runtime binds — never
  a cloudbox public URL.
- **Fail-closed namespace admission.** `cluster.allowed_namespaces` is the
  peer-local admission policy, enforced fail-closed: a pod whose namespace is
  not listed is refused. An **empty** list denies every pod, on purpose — a peer
  plane has no cloudbox authority to consult, so the operator must declare
  policy rather than have the node silently accept every workload. (On the
  cloudbox plane the allow-set is still fetched and refreshed from cloudbox.)

`outpost cluster leave` clears these peer-only fields
(`client_cert`/`client_key`/`allowed_namespaces`) along with the other peer
membership fields; `cluster.ca` and `cluster.token` are left for the cloudbox
re-fetch to overwrite.

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

## 5. Leave a worker

On the **tunnelled worker**, leave the peer-hosted plane with:

```bash
outpost cluster leave --yes
```

For a peer-joined worker this:

- clears only the local peer membership fields — `cluster.join_endpoint`,
  `cluster.join_token`, `cluster.stcp_secret`, and `cluster.node_token`;
- disables cluster mode and stops the local runtime container on the ensuing
  restart, preserving the selected runtime set so a rejoin returns as the same
  kind of node;
- **preserves the local overlay (Tailscale) identity** rather than purging it.
  Leave never contacts the peer plane or any overlay registrar — the worker
  holds no admin credential to deregister anything remotely — so no overlay
  deregistration ever occurred. Purging the machine key here would desync it
  from a registration that still exists wherever the peer plane's overlay is
  registered; a cloud-managed leave purges instead, because cloudbox has
  already deregistered that node from Headscale itself (see the boundary note
  below);
- leaves cloudbox pairing and every unrelated app, shell, LLM, outbound, and
  mesh setting untouched. Leaving the cluster is not logging out of the portal.

Without `--yes` the command prints a plane-appropriate confirmation and does
nothing else, so it is safe to run to see what it would do.

The command is idempotent: running it on a worker that has already left is a
harmless no-op that requests no restart. It is recoverable: rejoin later by
re-running the full `outpost cluster join <endpoint> ...` with credentials read
again from the control-plane host.

### Boundary: the Kubernetes Node object is not deleted by the worker

Leaving does **not** delete this worker's `Node` object from the peer plane's
apiserver, and it does not contact cloudbox to reclaim anything — cloudbox never
issued a peer-joined node. The worker holds only a k3s **join token**, which is
not an admin credential for the peer apiserver, and leave deliberately does not
add one to a worker. Removing the stale `Node` object is the **control-plane
host's** garbage-collection responsibility.

The control-plane host's outpost daemon does this automatically. Alongside the
other control-plane reconcilers (which require
`cluster.control_plane_kubeconfig` to point at an admin kubeconfig for the
hosted plane), a garbage collector runs every hour and deletes Node objects
that are all of:

- labelled exactly as an outpost k3s agent node
  (`outpost.dhnt.io/runtime=agent`, `outpost.dhnt.io/backend=k3s`, and a
  nonempty `outpost.dhnt.io/host` whose value prefixes the node name — the
  identity the k3s agent entrypoint stamps at registration);
- `NotReady` or `Unknown` for **longer than 24 hours**, per the Ready
  condition's own last-transition timestamp — no cloudbox liveness feed is
  consulted, so the collector works fully offline.

Everything else is out of scope by construction: Ready nodes,
virtual-kubelet nodes (`runtime=virtual`), foreign or unlabelled nodes, nodes
whose name does not match their host label, and nodes missing readiness
evidence are never touched. Deletes are capped at 3 per **hour of wall clock**
(oldest first), each node is re-checked with a fresh read immediately before
its delete, the delete carries a UID precondition so a same-name rejoin
registered in between survives, and any apiserver error aborts the whole pass.
A machine that comes back inside the 24 h window simply reconnects and goes
Ready again — nothing is lost by waiting a day.

The same "absence of evidence is not staleness" rule is enforced at fleet
level — this code deletes cluster state unattended, so it is built to refuse
rather than guess:

- **Mass-partition circuit breaker.** When more than half of the observed
  outpost-agent population is stale at once (and at least two nodes are), the
  whole pass is refused: everything looking dead at the same instant is the
  signature of the *observer* — this host's network, tunnel, or clock — being
  broken, not of the fleet dying. A single stale node always stays reapable,
  so a one-worker cluster still converges. This mirrors the upstream
  node-lifecycle controller's unhealthy-zone threshold. The fraction's
  denominator is the **eligible** population — the nodes that could actually
  be deleted — so structurally-excluded nodes (the plane's own agent nodes,
  control-plane-role nodes) do not dilute it; otherwise a handful of dead
  self-host ghosts could let a 100%-of-workers partition compute a sub-half
  fraction and drain the whole worker fleet. Note the breaker is an
  *instantaneous* fraction, not a running drain limit: it fully protects a
  fleet only while stale nodes are **above** the threshold at the same
  instant. A partition of, say, 49% of a 100-node fleet stays below the line
  and is still drained at 3/hour over roughly a day — defensible given the
  24 h grace (a genuinely dead node has been gone a day), but the guarantee is
  "no mass drain in one pass", not "no drain of a large minority ever".
- **Restart-safe delete rate, fail-closed.** The 3-per-hour budget is anchored
  to wall clock in a persisted ledger, not to the process: outpost
  self-restarts on every builtin toggle, and restarts must share one budget,
  not mint one each. If that durable ledger cannot be established at all (no
  writable cache dir), a real pass **deletes nothing** — an absent rate limit
  is treated as a reason to refuse, never as an unlimited budget. There is
  also no immediate pass at boot — the first pass waits out a settle window
  (5 min).
- **Clock-skew refusals, both directions.** Staleness is wall-clock arithmetic
  against apiserver-recorded timestamps. If any in-scope node's readiness
  evidence is timestamped in the collector's future (beyond a 5 min
  tolerance), the pass is refused — the clocks provably disagree. The
  *forward* direction is guarded too: between passes the collector
  cross-checks how much wall-clock time elapsed against its own monotonic
  uptime, and refuses the pass when the wall clock raced ahead (a resumed VM,
  a restored snapshot, a dual-boot RTC, a delayed NTP step) — a forward jump
  leaves no future timestamps, it just makes every past transition look older,
  which would otherwise reap a briefly-down node as long-dead. Deletions
  additionally require an hour of continuous collector uptime (measured
  monotonically), so a host whose RTC was wrong at boot runs read-only passes
  until NTP has had time to correct it. Once the wall clock settles at its new
  value the collector re-syncs its baseline and resumes.
- **Structural exclusions.** A node whose host label claims the control-plane
  host's own identity, or that carries a
  `node-role.kubernetes.io/control-plane` / `master` role label, is never a
  candidate — reaping the plane's own node is strictly worse than reaping a
  worker. The own-identity match is keyed on the host's **`agent_name`** (what
  the k3s entrypoint stamps into `outpost.dhnt.io/host`), not on any
  `cluster.node_name` override. One consequence of the host-label prefix rule
  is that a worker registered under a `cluster.node_name` that diverges from
  its `agent_name` does not match the `<host>-<id>` name pattern and so is
  **invisible to GC** — its stale Node object must be reclaimed by hand. This
  is a conservative bias (GC only ever acts on nodes whose name and labels
  agree), accepted here rather than widened.

Every deletion and every refusal is appended as one JSON line to
`<UserCacheDir>/outpost/nodegc.log` (same JSONL-ledger pattern as
`upgrade.log`), so "what did GC do and why" survives daemon restarts and log
rotation. Deletion lines are inherently bounded (at most 3 per hour), and a
**repeated identical refusal is not re-recorded** — a partition that persists
for days refuses every pass but writes only one refusal line, so the ledger
does not grow per-pass while the budget check re-reads it. (A restart re-writes
one refusal line, bounded by the settle window.)

**Ownership scope (recorded decision).** The candidate filter trusts node
*labels*, which are self-asserted by the joining kubelet; the plane holds no
per-node owner record, and the collector's charter is to work fully offline.
Under multi-tenancy (dhnt/docs/dks-tenancy-model.md) a plane may admit
workers joined by other owners, and a dead node they labelled as an outpost
k3s agent will eventually be reaped here. That is accepted: the plane host is
the tenancy authority for the plane it hosts, deletion additionally requires
more than the 24 h grace of NotReady (labels alone can never get a live node
reaped), and a reaped Node object is cheap to re-mint by rejoining. What the
filter must never do — delete the plane's own node or a control-plane node —
is excluded structurally, per above, rather than by label trust.

**Switches.** `OUTPOST_NODEGC` is read as two **allowlists**, never
fail-open. Only `live` (or blank/unset, the default) authorizes real deletion;
only `dry-run` (also `dryrun` / `dry_run`) enters dry run, which computes,
logs, and ledgers exactly what a real pass would delete — including budget and
breaker decisions — without deleting anything. **Anything else disables the
collector entirely** — `off`, `false`, `0`, `no`, `disabled`, or a typo like
`of` or `dryrunn`. This is deliberate: a mistyped disable must never leak into
live deletions, and a mistyped dry-run must never delete for real. Disabling
leaves the kubeconfig untouched and the other reconcilers running.

These switches are **env-only for now**, which is a departure from the repo's
four-surface convention (file key + REST + MCP + CLI). It is a conscious
deferral, not an oversight: wiring the toggle through `admincore.SetBuiltins`
touches the admincore / MCP / config packages, and the change that hardened
this collector was scoped to the collector itself. It is tracked to land as a
`cluster.node_gc` field routed through `admincore.SetBuiltins` like every other
toggle once the cluster config surface settles. Until then, the off switch for
unattended node deletion is this one environment variable — and because the
unrecognized-value default is *disable*, the fail-safe direction is preserved
even without the richer surface.

A worker that left (or died) therefore disappears from `kubectl get nodes` on
its own after the grace period. To reclaim it sooner, delete it explicitly on
the control-plane host:

```bash
kubectl --kubeconfig ~/.kube/outpost-control-plane/k3s.yaml delete node <worker-node-name>
```

This is the intentional difference from the cloud-managed leave path. A
cloud-managed node's `outpost cluster leave` POSTs `/api/v1/cluster/leave` so
cloudbox reclaims the k8s Node, overlay registration, and pod-CIDR reservation
it issued. A peer worker skips that reclaim entirely because those resources
live on a machine cloudbox does not control.

## 6. Recover

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

## 7. Limits

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
