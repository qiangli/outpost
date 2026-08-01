# ADR: Peer-Hosted DKS Pod Network — Tailscale+Flannel vs outpost-cni+Routes

## Question

For a peer-hosted DKS cluster (per-user k3s control plane hosted on a user
machine via `outpost cluster control-plane on`; cloudbox remains the
tunnel/rendezvous coordinator and the Headscale host for the tailnet), which
pod-network path do peer-joined worker nodes use:

- **A)** Tailscale as the node underlay, with stock k3s flannel VXLAN pinned
  to it (`--flannel-iface=tailscale0`), or
- **B)** the existing custom `outpost-cni` plugin plus Tailscale advertised
  subnet routes (the shape the cloudbox-hosted overlay uses today)?

Nothing else is in scope. No cloudbox code changes are proposed by either
option.

## Option A — Tailscale underlay + flannel VXLAN

Every peer-joined node runs `tailscale up` against the existing Headscale
host (cloudbox) to obtain a tailnet IP, and k3s agent is started with
`--flannel-iface=tailscale0` (a documented k3s **agent** flag — upstream
K3s docs, cited externally, not repo-verifiable). Flannel's VXLAN backend
reads `Node.spec.podCIDR` off each Node and encapsulates pod traffic
addressed to the node's advertised IP — here, the tailnet IP, because
`--flannel-iface` selects it.

Two properties make this decisive, not merely workable:

- **Flannel consumes `Node.spec.podCIDR` directly.** k3s's embedded
  controller-manager (on whichever peer hosts the control plane) allocates
  and writes `podCIDR` onto each Node as part of normal k3s bootstrap — the
  same mechanism every stock k3s cluster relies on. This eliminates gap B5
  (a custom pod-CIDR watcher/bootstrap/allocator and hand-templated
  conflist) **entirely** — no bespoke component left to build or operate on
  the pod-CIDR path.
- **Tailscale supplies reachable node IPs, so VXLAN crosses NAT.** Flannel
  VXLAN's normal blocker is node-to-node IP reachability (two home NATs,
  neither port-forwarded). A tailnet address is reachable from every other
  tailnet member outpost already pairs with — exactly what a tunnelled
  worker's container-local/eth0 address is **not**
  (`internal/agent/runtime/image/entrypoint.sh:405-414` explains why the
  runtime avoids advertising that address as the Node's real IP today).
  Pinning flannel to `tailscale0` reuses that already-working reachability
  property instead of re-deriving it.

Peer join under A needs a fresh Headscale login/authkey so the node gets a
tailnet identity, but it does **not** need to advertise or have cloudbox
approve any pod subnet route — flannel's VXLAN carries pod traffic itself;
Tailscale is only the node-to-node underlay.

## Option B — outpost-cni + advertised routes

Every peer-joined node runs the existing `outpost-cni` CNI plugin
(`internal/agent/runtime/image/cni/`) which builds a per-node bridge
(`cbox0`) from a `/24` carved for that node, and Tailscale is asked to
**advertise** that `/24` as a subnet route, which another tailnet member
must **approve** before traffic routes to it. This is the shape the current
cloudbox-hosted overlay path uses
(`internal/agent/runtime/image/entrypoint.sh:70-106`,
`internal/agent/runtime/podnet.go:16-22`).

Peer join under B needs everything A needs (a fresh Headscale login)
**plus** a subnet-route advertise/approve step per node. The approval
authority is the load-bearing problem: cloudbox's existing route-approval
logic is bound to its own per-host pairing record (the cloud-hosted
overlay, where cloudbox carves `PodCIDR` at Exchange time — see
`internal/agent/runtime/runtime.go:126-129`). A peer-hosted cluster's
`Node.spec.podCIDR` is allocated by the **peer's** k3s controller-manager,
not cloudbox — there is no cloudbox-side record mapping a peer cluster's
per-node `/24` to an approvable route. B needs either a new cloudbox-side
approval surface keyed to peer-cluster pod CIDRs (cloudbox change, out of
scope) or a manual/out-of-band approval step per node, indefinitely,
drifting from what the peer's allocator actually hands out. This mismatch
is the core liability of Option B.

## What each requires that does not exist today

Baseline, true for **both** options:
`internal/agent/admincore/cluster_join.go:159-161` clears
`fc.Cluster.OverlayLoginServer`, `OverlayAuthKey`, `OverlayPodCIDR`
unconditionally on every peer join (`JoinPeerPlane`). A peer-joined worker
today has **zero** overlay/Tailscale credentials — the runtime container
never calls `tailscale up` because `runtime.go:132-134`'s
`OverlayLoginServer`/`OverlayAuthKey` are both empty, and it falls into the
single-node CNI branch (`entrypoint.sh:107-134`) regardless of the
(also-empty) `PodCIDR`. A claim that "the overlay path already works for
peer join, only validation remains" is false — this is an absent
credential-provisioning path, not a validation gap. Both options must add:
(a) a way for `JoinPeerPlane` to populate
`OverlayLoginServer`/`OverlayAuthKey` (or a peer-scoped equivalent) instead
of zeroing them, and (b) plumbing those into the peer-join path the way
`runtime.Options` already does for the cloudbox-hosted plane.

**Option A additionally needs:**
- `--flannel-iface=tailscale0` threaded into the k3s agent/server exec in
  `entrypoint.sh:446-461` — PROPOSED; today's exec passes no
  `--flannel-iface`/`--flannel-backend` flag at all.
- A guard so the `else` branch of `entrypoint.sh:107-134` (`10-bridge.conflist`) is **skipped** in peer-flannel mode: flannel writes its own `/etc/cni/net.d/10-flannel.conflist` once started, and CNI picks the lexically-first `.conflist` in that directory — `10-bridge` sorts before `10-flannel`, so a leftover `10-bridge.conflist` would win and flannel would never be selected. Neither `10-outpost.conflist` nor `10-bridge.conflist` may be written in peer-flannel mode; a new mode selector must gate all branches of that `if/else` — PROPOSED.
- A `PodNetworkMode` classification for this mode in `internal/agent/runtime/podnet.go:16-30` (today only `PodNetworkOverlay` and `PodNetworkSingleNodeFallback` exist) — PROPOSED.

**Option B additionally needs:** everything the credential-provisioning delta above requires, **plus** a cloudbox-side (or manual, per-node, indefinite) mechanism to approve a peer cluster's per-node `/24` as a subnet route — no such mechanism exists in this repo and none is proposed here, per the constraint against cloudbox changes — **plus** re-pointing `outpost-cni`'s pod-CIDR supply (today: cloudbox's Exchange-time carve, `internal/agent/runtime/runtime.go:126-129`) at the peer's k3s controller-manager's `Node.spec.podCIDR` output instead — new plumbing, PROPOSED. Option A needs none of this because flannel already reads `Node.spec.podCIDR` natively.

## Security

**Option A (tailnet-wide VXLAN reachability):** every node on the tailnet
flannel is pinned to can, in principle, send VXLAN frames to any other
member's `tailscale0` address. The attack surface is "reachable by anything
Headscale has authenticated onto this tailnet" — bounded by Headscale ACLs
(not evaluated here), not by per-pod-CIDR granularity. This is the same
reachability model the cloudbox-hosted overlay already grants today — A
does not widen it, it reuses it via a different CNI backend.

**Option B (per-subnet approval, but broken for peer clusters):** in
principle, per-`/24` route approval is finer-grained than "any tailnet
member reaches any VXLAN peer." In practice, per "What each requires"
above, no approval authority exists for a peer-hosted cluster's pod CIDRs
without a new cloudbox surface, out of scope here. Standing up B today
means either unapproved routes (packets silently dropped, cluster
non-functional) or manual approval of indefinitely-drifting `/24`s with
nothing tying approval to the peer's actual allocator output — a control
that exists on paper but cannot be operated correctly with current
tooling. A theoretical finer-grained model that cannot be provisioned is
not a security advantage.

## Decision

**Choose Option A: Tailscale underlay + stock k3s flannel VXLAN
(`--flannel-iface=tailscale0`).**

1. It eliminates gap B5 entirely — flannel already speaks
   `Node.spec.podCIDR`, so peer-hosted DKS gets pod networking from k3s's
   own allocator with no bespoke watcher/bootstrap/conflist code.
2. It has no unimplementable dependency — B's per-subnet approval needs
   cloudbox-side work this ADR is barred from proposing, making B
   un-buildable within scope; A's only new work (credential provisioning +
   a `--flannel-iface` flag + skipping two conflist writes) is entirely
   within `internal/agent`.
3. It reuses an already-proven reachability property (tailnet addresses
   already cross NAT for this codebase's overlay use case) instead of a
   second, harder-to-provision access-control layer.

**Strongest counter:** per-subnet route approval (B) is a tighter security
boundary in principle — an operator scopes exactly which pod CIDRs are
reachable, rather than granting tailnet-wide VXLAN reachability. **Why it
loses:** that finer boundary cannot be correctly operated for a
peer-hosted cluster without a cloudbox-side approval surface bound to the
peer's own `Node.spec.podCIDR` allocation, which does not exist and is out
of scope to build here. A security control that cannot be provisioned is
not available; the smallest secure route to running workloads is the one
actually implementable within this repo today.

## Proof boundary

Grep-confirmed in `cmd/outpost/cluster*.go` (run on two machines — a peer
host running the control plane, and a worker joining it):

```
# On the peer control-plane host:
outpost cluster control-plane on           # cmd/outpost/cluster_control_plane.go:40
outpost cluster control-plane token        # cmd/outpost/cluster_control_plane.go:111
outpost cluster token                      # cmd/outpost/cluster_node_token.go:22

# On the worker (peer join, existing CLI):
outpost cluster join <endpoint> \
  --token <join-token> --stcp-secret <secret> --node-token <k3s-node-token>
                                            # cmd/outpost/cluster.go:108-180
outpost cluster join --show                # cmd/outpost/cluster.go:141
outpost cluster join --clear               # cmd/outpost/cluster.go:143

# On either, to inspect the resulting cluster:
outpost cluster kubeconfig                 # cmd/outpost/cluster.go:372
outpost kubectl -- get nodes               # cmd/outpost/kubectl.go
```

**PROPOSED (does not exist today)** — needed to carry Option A's tailnet
credentials through a peer join; not grep-able in `cmd/outpost/` or
`cluster_join.go` today:

```
outpost cluster join <endpoint> --overlay-login-server <hs-url> \
  --overlay-auth-key <ephemeral-authkey>    # PROPOSED — no such flags exist on cluster.go's join command
```

Confirming pod-to-pod reachability across two peer-joined nodes (the actual
flannel-over-tailscale VXLAN path) requires two physical or two VM machines
on separate networks — not verifiable from a single-machine checkout and
not claimed as verified here.

## Consequences

- `internal/agent/runtime/podnet.go:16-30`'s `PodNetworkMode` enum (today: `PodNetworkOverlay`, `PodNetworkSingleNodeFallback`) gains a third value (e.g. `PodNetworkPeerFlannel`) so status/logging can distinguish flannel-over-tailscale from both existing modes.
- The conflist-writing block in `entrypoint.sh:94-134` gains a guard: under peer-flannel mode, **neither** `10-outpost.conflist` **nor** `10-bridge.conflist` may be written — flannel writes its own `10-flannel.conflist` once started, and a leftover file from either existing branch would out-select it under CNI's lexical-order pick.
- No cloudbox code changes accompany this decision; Option B was rejected specifically because it would have required one.
- `outpost-cni` (`internal/agent/runtime/image/cni/`) is not deleted — it remains the CNI path for the existing cloudbox-hosted overlay, unaffected by this ADR.

## Citations

- K3s official docs, agent CLI reference — `--flannel-iface` is a k3s agent flag. UNVERIFIED against this repo (no vendored k3s source); given as a REQUIRED FACT, not re-derived.
- `internal/agent/runtime/image/Dockerfile:67` — `ARG K3S_VERSION=v1.36.1+k3s1`.
- `internal/agent/runtime/image/entrypoint.sh:70-134` — current CNI conflist branches (`10-outpost.conflist` / `10-bridge.conflist`).
- `internal/agent/runtime/image/entrypoint.sh:405-414,446-461` — node-IP / exec rationale for the k3s agent invocation; no `--flannel-iface` today.
- `internal/agent/runtime/podnet.go:16-30` — `PodNetworkMode` enum, two values today.
- `internal/agent/runtime/runtime.go:126-134` — cloudbox-carved `PodCIDR` and overlay-credential fields for the cloudbox-hosted plane.
- `internal/agent/admincore/cluster_join.go:159-161` — peer join clears `OverlayLoginServer`/`OverlayAuthKey`/`OverlayPodCIDR` unconditionally.
- `internal/agent/mesh/host.go:107` — `EnableAutoRelayWithStaticRelays` (relay **client** only); `EnableRelayService` appears solely in `internal/agent/mesh/relay_test.go:28,79`, not the mesh host used here — cited to preempt reuse as a relay-service claim.
- `cmd/outpost/cluster.go:108-180` — `outpost cluster join` flags and peer-join branch.
- `cmd/outpost/cluster_control_plane.go:40,111` — `control-plane on` / `control-plane token`.
- `cmd/outpost/cluster_node_token.go:22` — `cluster token`.
