# novicortex → Dragon peer DKS migration — BLOCKED

Operational record for migrating **novicortex only** into the Dragon-hosted
peer DKS control plane as a real k3s agent.

- Run date: 2026-08-02
- Control-plane host: **dragon**, context `dragon`, endpoint
  `https://127.0.0.1:16443`
- Target worker: **novicortex**
- Outcome: **BLOCKED — novicortex was not joined.** No migration was performed.

**Nothing in this document claims the migration succeeded.** The single
blocker below stopped the run before any join was attempted, and every
downstream objective (kubelet Ready, unique PodCIDR, default-context switch,
two-real-worker acceptance) is therefore **unmet**.

This document contains no kubeconfig content, no tunnel/STCP/node token, no
vk credential bundle, and no elevation cookie value. No ad-hoc credential
utility was written; only the documented surfaces in `docs/cluster-peer.md`
were used.

---

## 1. Blocker (exact)

**Every execution path onto novicortex requires a fresh cloudbox elevation,
and minting one requires novicortex's interactive OS password, which this
non-interactive run does not have.**

`outpost cluster join` must run **on the worker** (`docs/cluster-peer.md` §4).
Reaching novicortex to run it failed identically on all three client paths:

| Command | Result (verbatim) |
| --- | --- |
| `outpost ssh novicortex -- 'hostname; id -un'` | `INFO ssh: cloudbox-assisted dial failed; trying LAN-direct password path host=novicortex err="re-elevate novicortex: no TTY for password prompt"` then `Error: re-elevate novicortex: no TTY for password prompt` |
| `outpost ssh exec mk -- hostname` (saved target `mk` → `noviadmin@novicortex`) | `Error: elevation required for "novicortex" — run `outpost connect novicortex`` |
| `outpost ssh-proxy novicortex` | `Error: re-elevate novicortex: no TTY for password prompt` (exit 1) |

### Why the cached cookie did not cover it

`~/.cache/outpost/sessions/novicortex.cookie` exists but was last written
**2026-07-30 06:38**, ~85 h before this run — far past the documented 8 h
absolute elevation TTL. Its value was not read or reproduced.

`outpost connect novicortex --cookie-only </dev/null` **does** return
`Keep-alive (cookie-only): pinging every 20 min until SIGTERM or absolute
expiry` and exit 0, but this is not evidence of a usable session: the
cookie-only keep-alive adopts the cached file without validating it, and the
SSH client immediately afterwards still decided to re-elevate. Treating that
exit 0 as proof of access would be exactly the absence-of-evidence-as-success
failure `docs/fleet-evidence-invariant.md` forbids.

This is **not** host-specific and **not** a novicortex defect: the same
re-elevation demand reproduces against `novidesign`
(`Error: re-elevate novidesign: no TTY for password prompt`), whose cookie is
also past absolute expiry. Every cached elevation available to this run is
stale.

### Why no fallback path closed it

| Fallback | Status |
| --- | --- |
| LAN-direct SSH (`$OUTPOST_SSH_PASSWORD`, non-interactive) | Unavailable — variable unset, and no LAN route exists: `outpost scan` → `No outpost peers found on the LAN within the timeout`; `outpost ssh noviadmin@novicortex.local` → `lan-direct dial novicortex.local:2222: dial tcp: lookup novicortex.local: no such host` |
| Cached remote MCP credentials (`outpost remote login`) | `No cached remotes.` |
| Mesh forward to novicortex | Mesh is not enabled on dragon (`outpost status --json` → `mesh: null`); and a mesh-carried SSH session authenticates with the same OS password |
| Run the join from dragon instead | Not possible by design — `outpost cluster join` persists worker-side `agent.json` fields and restarts the worker's daemon |

Obtaining the password by any other means was out of scope: the task forbids
ad-hoc secret-mint utilities, and harvesting an operator credential is not a
documented surface.

**Connectivity is not the blocker.** `outpost reach novicortex` classifies the
host as reachable: `{"host":"novicortex","route":"cloudbox","rtt_ms":62,
"endpoint":"https://ai.dhnt.io", ...}`, exit `10` (= cloudbox route, not
`20`/offline). novicortex is also present in `outpost ssh-config`'s visible-host
list, so cloudbox still knows the pairing. The gap is **authorization only**.

---

## 2. Verified state of the Dragon peer plane (read-only)

The hosting side is healthy and ready to accept a second worker. Verified with
the daemon binary at `/Users/qiangli/bin/outpost` (`4a179f5-dirty`, built
2026-08-02T19:08:04Z), which is the binary running `outpost start` on dragon.

> Note for anyone reproducing: the first `outpost` on `PATH`
> (`~/.local/bin/outpost`, `d5cd684`, built 2026-07-30) **predates the
> `outpost cluster control-plane` subcommand** and silently prints the
> `cluster` help instead of a status. Use the daemon's own binary.

`outpost cluster control-plane status`:

```text
hosted: yes
container exists:    true
container running:   true
apiserver serving:   true
apiserver status:    401
join endpoint:       0.0.0.0:7000
credentials:         join_token=true node_token=true stcp_secret=true
node count:          1
nodes:
  novidesign-4c7adeb6: ready
```

All three join credentials are present on the plane (presence booleans only —
no value was printed, and `outpost cluster control-plane token` /
`outpost cluster token` were **not** run, since there was no worker to deliver
them to).

Existing worker, from the Dragon peer kubeconfig (`kubectl --context dragon`):

| Fact | Value |
| --- | --- |
| Node | `novidesign-4c7adeb6` |
| Status | `Ready` (age 38 h) |
| Kubelet | `v1.36.1+k3s1` — real kubelet |
| Runtime | `containerd://2.2.3-k3s1` |
| `.spec.podCIDR` | `10.42.0.0/24` |
| Labels | `outpost.dhnt.io/runtime=agent`, `outpost.dhnt.io/backend=k3s`, `outpost.dhnt.io/host=novidesign`, `outpost.dhnt.io/runtime-ready=true` |
| `flannel.alpha.coreos.com/public-ip` | **present**, and inside the `100.64/10` tailnet CGNAT range |

The plane is therefore the **ADR Option A** topology this gate exists to accept
(Tailscale node underlay + stock k3s flannel VXLAN), not a cloudbox-hosted
cluster. novicortex is **absent** from `kubectl --context dragon get nodes`, as
the issue states.

---

## 3. Acceptance subset — verbatim

The requested two-real-worker nodes/PodCIDR subset was run against the Dragon
peer kubeconfig. It cannot pass with one worker; this is the pre-migration
baseline, recorded so the delta after a successful join is unambiguous.

```text
$ KUBECONFIG=~/.kube/config DKS_ONLY=nodes-ready,distinct-pod-cidrs \
  DKS_TIMEOUT=90 bash script/dks-peer-acceptance.sh

NOTE ready_real_nodes=1 node_a=novidesign-4c7adeb6 node_b=none
NOTE ready_real_node_list=novidesign-4c7adeb6
CHECK nodes-ready BLOCKED requires two Ready kubelet-backed nodes in one cluster; found 1 (novidesign-4c7adeb6 ); a one-node venue proves nothing either way
CHECK distinct-pod-cidrs PASS 1 distinct podCIDRs: 10.42.0.0/24

SUMMARY pass=1 fail=0 blocked=1 attested=0 substantive_pass=0 venue=NOT-RUN
RESULT INCONCLUSIVE (only precondition checks passed; no substantive check proved anything; the peer venue was never asserted (peer-venue did not run; DKS_ONLY cannot deselect it out of the verdict))
```

Runner exit: **2** (INCONCLUSIVE) — correct. `nodes-ready` BLOCKED is the direct
consequence of the blocker in §1.

`peer-venue` was also run on its own:

```text
$ KUBECONFIG=~/.kube/config DKS_ONLY=peer-venue DKS_TIMEOUT=90 \
  bash script/dks-peer-acceptance.sh

CHECK peer-venue BLOCKED the node-annotation query returned nothing for: novidesign-4c7adeb6 ; the venue could not be identified either way
SUMMARY pass=0 fail=0 blocked=1 attested=0 substantive_pass=0 venue=BLOCKED
RESULT INCONCLUSIVE (no check passed; the peer venue could not be asserted (peer-venue BLOCKED) — 'we could not tell where we are' is not a licence to certify what we found there)
```

Runner exit: **2**.

### Observation: `peer-venue` BLOCK here is a one-node harness artifact, not a venue defect

`peer-venue` queries `kubectl get nodes $READY_LIST -o jsonpath={range
.items[*]}...` (`script/dks-peer-acceptance.sh:745`). With a **single** node
name, kubectl returns a bare `Node` object rather than a `List`, so `.items[*]`
matches nothing and the check BLOCKs. Reproduced directly:

```text
# single name → empty (rc 1)
kubectl get nodes novidesign-4c7adeb6 -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{...public-ip}{"\n"}{end}'

# same node, no .items → the annotation is there
kubectl get nodes novidesign-4c7adeb6 -o 'jsonpath={.metadata.name}{" "}{...public-ip}'
novidesign-4c7adeb6 <tailnet address in 100.64/10>
```

Impact is low and this run did not patch it: a one-node venue is already
BLOCKED by `nodes-ready`, and with two or more node names kubectl returns a
List and the query works. It is recorded because it would otherwise be
misread as "the peer plane is not ADR Option A", which §2 shows it is.

---

## 4. Objectives status

| Objective | Status |
| --- | --- |
| novicortex joined to Dragon peer DKS as a real k3s agent | **NOT DONE** — blocked at §1 |
| novicortex real kubelet `Ready`, proven from Dragon peer kubeconfig | **NOT DONE** — node absent from the plane |
| novicortex unique `.spec.podCIDR` vs `novidesign-4c7adeb6` | **NOT DONE** |
| novicortex local default context = peer, named cloudbox fallback retained | **NOT DONE** — requires shell access to novicortex |
| Two-real-worker nodes/PodCIDR acceptance subset green | **NOT MET** — exit 2, `substantive_pass=0` (§3) |
| Dragon plane verified able to accept the worker | **DONE** (§2) |
| Redacted public-safe report committed | **DONE** (this file) |

No production or harness code was modified.

---

## 5. What unblocks this

One thing: an authenticated session to novicortex. Either

- an operator runs `outpost connect novicortex` **on a TTY** (or
  `outpost connect novicortex --stdin` feeding the password) to mint a fresh
  `matrix_elev` cookie — `--keep-alive` is advisable so the session survives
  the run; or
- novicortex is reachable LAN-direct and `$OUTPOST_SSH_PASSWORD` is exported
  for the non-interactive path.

Then the documented sequence, unchanged from `docs/cluster-peer.md` §3–§4,
piping credentials so no value lands in shell history or a process listing:

1. On **dragon**, read the endpoint + tunnel token + STCP secret
   (`outpost cluster control-plane token --quiet`) and the k3s node token
   (`outpost cluster token`). These are three distinct credentials; do not
   substitute one for another.
2. On **novicortex**, `outpost cluster join <dragon-endpoint>:7000` with
   `--token` / `--stcp-secret` / `--node-token`. Naming neither runtime flag
   keeps the default single real k3s agent, which is what this migration wants
   — do **not** pass `--cluster-virtual`, and no vk bundle is required.
   novicortex must be **paired with cloudbox** for the agent runtime: its pod
   network is flannel VXLAN over the Tailscale underlay and the tailnet auth
   key is fetched over the pairing's access token.
3. Wait for the worker's daemon to restart, then verify **from dragon**:
   `kubectl --context dragon get nodes -o wide` — expect a second `Ready`
   kubelet-backed node with a `.spec.podCIDR` distinct from `10.42.0.0/24`.
4. On **novicortex**, set the peer context as current-context in its default
   kubeconfig, keeping the named `cloudbox` context as fallback.
5. Re-run §3's subset. With two Ready workers, `nodes-ready` should PASS,
   `distinct-pod-cidrs` should report 2 distinct CIDRs, and `peer-venue`
   should assert cleanly (the §3 artifact does not apply at two nodes).
   Note that this subset alone still exits `2` — both checks are
   **preconditions**; a green run needs a substantive check
   (`docs/peer-dks-hardware-acceptance.md`, *Check taxonomy*).
