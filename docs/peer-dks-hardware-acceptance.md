# Peer-Hosted DKS Pod Networking — Hardware Acceptance

Acceptance record for the peer-flannel slice (ADR Option A: Tailscale node
underlay + stock k3s flannel VXLAN pinned via `--flannel-iface=tailscale0`;
see `docs/adr-peer-dks-pod-network.md`).

**Headline: cross-node pod networking on the peer-hosted DKS path is
UNPROVEN.** No peer-hosted DKS cluster was reachable during this run — the only
cluster available is the **cloudbox-hosted overlay** cluster, which does not run
flannel at all, so the shipped peer-flannel code path was never exercised on
hardware. Separately and independently, cross-node pod-to-pod IP reachability
**FAILS on that cloudbox-hosted cluster** (100% packet loss, reproduced across
three runs). That failure is real evidence about the venue that was available;
it is *not* evidence about the peer-flannel slice either way.

- Harness: `script/dks-peer-acceptance.sh`
- Offline tests: `script/dks-peer-acceptance_test.sh`
- Run date: 2026-08-01

---

## Venue inventory (Phase 1 — read-only, presence-only)

Gathered with read-only, already-authenticated commands. **No secret value is
reproduced here.** `outpost status --json` was executed once and observed to
emit app `provisioning_token` / `sso_secret` values in cleartext; its output was
therefore **not** captured to any file and is reported presence-only below.
`outpost cluster join --show` was **not run**, per the secret-hygiene rule.

| Fact | Value |
| --- | --- |
| kubectl | present, `/usr/local/bin/kubectl` |
| outpost | present, `/Users/qiangli/.local/bin/outpost` |
| podman | present; docker absent; **tailscale CLI absent on this host** |
| kubeconfig contexts | `cloudbox` (current), `dragon` |
| current context | `cloudbox`, namespace `user-5c6b0a233188` |
| local outpost identity | `agent_name` present; `has_token: false` |
| local outpost cloudbox | `https://ai.dhnt.io`, protocol `wss` |
| RBAC in namespace | `create pods` = yes, `get nodes` = yes |
| Two machines available? | **Yes — but not of the right kind** (see below) |

Nodes visible via the `cloudbox` context (`kubectl get nodes -o wide`):

| Node | Kind | Status | podCIDR |
| --- | --- | --- | --- |
| `dragon-75bd60ee` | k3s agent (`v1.36.1+k3s1`, arm64) | Ready | `10.42.0.0/24` |
| `puppy-3a07cd18` | k3s agent (`v1.36.1+k3s1`, amd64/WSL2) | Ready | `10.42.2.0/24` |
| `novidesign-4c7adeb6` | k3s agent | NotReady | `10.42.1.0/24` |
| `novicortex-c1493ce9` | k3s agent | NotReady, SchedulingDisabled | `10.42.5.0/24` |
| 13 × `*-vk-{native,podman,ollama}` | virtual-kubelet (`v0.1.0-vknode`) | mixed | mixed / `<none>` |

**Decisive venue finding.** Two Ready kubelet-backed nodes *do* exist
(`dragon-75bd60ee`, `puppy-3a07cd18`), so the multi-node checks could run. But
this is the **cloudbox-hosted** cluster: no node carries the
`flannel.alpha.coreos.com/public-ip` annotation, i.e. **flannel is not running
anywhere in it**. The peer-flannel slice under test (peer-hosted control plane,
`--flannel-iface=tailscale0`) is therefore **not deployed in the only venue
reachable**. Standing up one would require pairing/joining machines, which is
explicitly out of scope.

---

## Results (Phase 3 — live run, verbatim)

Command:

```
DKS_TIMEOUT=90 bash script/dks-peer-acceptance.sh
```

Runner exit status: **1** (correct — checks FAILed).
Summary line: `SUMMARY pass=4 fail=3 blocked=4` → `RESULT FAIL`

| # | Check | Result | Evidence / missing precondition |
| --- | --- | --- | --- |
| 1 | `nodes-ready` | **PASS** | `2 Ready kubelet-backed nodes: dragon-75bd60ee puppy-3a07cd18` |
| 2 | `distinct-pod-cidrs` | **PASS** | `4 distinct podCIDRs: 10.42.0.0/24 10.42.5.0/24 10.42.1.0/24 10.42.2.0/24` (kubelet-backed nodes only) |
| 3 | `flannel-iface` | **BLOCKED** | `no node carries annotation flannel.alpha.coreos.com/public-ip; flannel is not running on any node (peer-flannel path not deployed here)` |
| 4 | `no-stale-conflist` | **BLOCKED** | `needs host-filesystem access to /etc/cni/net.d on a peer-flannel node; set DKS_ALLOW_NODE_DEBUG=1 to permit 'kubectl debug node/<n>' (requires node-debug RBAC + a privileged-capable namespace)` |
| 5 | `cross-node-pod-ip` | **FAIL** | `dksacc-a@dragon-75bd60ee -> 10.42.2.17@puppy-3a07cd18: 3 packets transmitted, 0 packets received, 100% packet loss` |
| 6 | `service-clusterip` | **FAIL** | `dksacc-a -> clusterIP 10.43.25.178:8080: wget: download timed out` |
| 7 | `cluster-dns` | **FAIL** (flaky) | `** server can't find dksacc-svc.svc.cluster.local: NXDOMAIN` — see note below |
| 8 | `logs-exec` | **PASS** | `logs+exec ok against dksacc-b@puppy-3a07cd18` — the remote/tunnelled node |
| 9 | `headlamp` | **PASS** | `Headlamp headlamp/headlamp at 10.43.246.3:80 reachable (HTTP/1.1 404 from /)` |
| 10 | `nanochat` | **BLOCKED** | `DKS_NANOCHAT_IMAGE unset; no nanochat image is published in this repo and this harness will not substitute a weaker workload` |
| 11 | `bashy-chunked` | **BLOCKED** | `DKS_BASHY_IMAGE unset; no chunked-bashy container image or Job manifest exists in this repo, and there is no deterministic substitute for a distributed chunked workload` |

### Notes on individual results

- **#5 `cross-node-pod-ip` — reproduced 3/3 runs.** A busybox pod pinned to
  `dragon-75bd60ee` cannot reach a pod IP on `puppy-3a07cd18` at all. This is a
  genuine, repeated hardware failure of the *cloudbox-hosted* overlay
  (outpost-cni + advertised routes), observed on 2026-08-01. It says nothing
  about the peer-flannel path, which is not deployed here.
- **#6 `service-clusterip`** follows directly from #5 — the Service's only
  backend pod is on the remote node, so the timeout is the same underlying
  failure, not an independent one.
- **#7 `cluster-dns` is FLAKY across runs, not stably failing.** It PASSed on
  two runs (`short name … resolved to 10.43.67.221` / `10.43.109.83`) and
  NXDOMAIN'd on the final run. The most likely cause is a CoreDNS propagation
  race against a Service created seconds earlier, not a cross-node DNS defect.
  Recorded as FAIL because that is what the final run actually produced; it
  should not be read as a settled result.
- **#9 `headlamp`** returned HTTP 404 from `/`. The check asserts *Service
  reachability*, and any well-formed HTTP status line proves that; the path
  Headlamp serves its UI on is not a pod-network property. Note Headlamp pods
  run on **both** `dragon` and `puppy`, so this reachability may have been
  satisfied by the node-local replica and is **not** independent proof of
  cross-node Service routing.
- **#2 `distinct-pod-cidrs`** is scoped to kubelet-backed nodes. Virtual-kubelet
  nodes are excluded because they do not participate in pod networking; six of
  them carry no `podCIDR` at all, and `novicortex-c1493ce9` (real) shares
  `10.42.5.0/24` with `dragon-vk-native` (virtual). See *Defects found*.

---

## Code-testable automation vs external hardware blockers

### Proven offline (no cluster required)

`bash script/dks-peer-acceptance_test.sh` → **52 pass, 0 fail**. It asserts the
harness's own logic, including the two invariants this story turns on:

- a `BLOCKED` check never tallies as, or renders as, `PASS`;
- any `FAIL` makes the runner exit non-zero (verified: real run exited 1);
- all-blocked reports `INCONCLUSIVE`, never `OK` — a harness that skipped
  everything cannot read as green;
- distinct-CIDR comparison: duplicate, empty, missing-field, late-duplicate and
  zero-node inputs all return failure with the offending node named;
- tailnet CGNAT range boundaries (`100.63` / `100.128` correctly rejected);
- one-line-per-check record shape (embedded `\n` and `\r` collapsed);
- `DKS_ONLY` exact-matches and never prefix-matches;
- every one of the 11 named checks is actually implemented.

### Proven on the available (wrong-kind) hardware

Checks 1, 2, 8, 9 ran green against a real 2-node cluster. Checks 5, 6, 7 ran
and produced real failures. These exercise the harness end-to-end against a live
API server, so the runner itself is validated; only the *venue* is wrong.

### External hardware blockers — genuinely unproven

| Blocker | Blocks | What it needs |
| --- | --- | --- |
| No peer-hosted DKS cluster reachable | #3, #4, and the *meaning* of #5–#9 | Two machines joined to a **peer-hosted** control plane (`outpost cluster control-plane on` + `cluster join`), each with a Headscale-issued tailnet identity. Out of scope: provisioning/pairing was explicitly forbidden. |
| No host-filesystem access to any node | #4 | `kubectl debug node/<n>` RBAC + a privileged-capable namespace, or shell access on the node. Re-run with `DKS_ALLOW_NODE_DEBUG=1`. |
| No published nanochat image | #10 | A container image reference in `DKS_NANOCHAT_IMAGE`. None exists in this repo. |
| No chunked-bashy image or Job manifest | #11 | A container image reference in `DKS_BASHY_IMAGE`. None exists in this repo. |

**What would close this out:** run the same harness unchanged against a
peer-hosted cluster with `DKS_ALLOW_NODE_DEBUG=1`, plus the two image env vars.
No harness change is required — checks 3 and 4 are implemented and will assert,
not skip, the moment a flannel-bearing node is present.

---

## Defects found

Not fixed here — this story is forbidden from touching production code.

1. **Duplicate `podCIDR` across a real and a virtual node.**
   `novicortex-c1493ce9` (k3s agent) and `dragon-vk-native` (virtual-kubelet)
   both hold `10.42.5.0/24`. Whether the k3s controller-manager allocator and
   the vknode node registration draw from the same pool is worth confirming: if
   a virtual node can consume a CIDR the real allocator later hands out, the B5
   distinctness property is not actually guaranteed cluster-wide. Registration
   path: `internal/agent/vknode/node.go`.
2. **Six virtual-kubelet nodes carry no `.spec.podCIDR`** (all
   `novicortex-vk-*` and `novidesign-vk-*`). Benign if virtual nodes never route
   pods, but it means any naive cluster-wide `podCIDR` distinctness assertion
   fails; the harness scopes around it deliberately.
3. **`cross-node-pod-ip` fails on the cloudbox-hosted overlay** (100% loss,
   3/3 runs, `dragon-75bd60ee` → `puppy-3a07cd18`). This is the *existing*
   outpost-cni + advertised-routes path, i.e. Option B in
   `docs/adr-peer-dks-pod-network.md`. It is corroborating evidence for the ADR's
   choice of Option A, and a live defect in the current overlay in its own right.

---

## Reproducing

```bash
# Offline — no cluster needed.
bash script/dks-peer-acceptance_test.sh

# Full acceptance against a peer-hosted cluster.
KUBECONFIG=~/.kube/peer.yaml \
DKS_ALLOW_NODE_DEBUG=1 \
DKS_NANOCHAT_IMAGE=<image> \
DKS_BASHY_IMAGE=<image> \
  bash script/dks-peer-acceptance.sh

# A single check.
DKS_ONLY=cross-node-pod-ip bash script/dks-peer-acceptance.sh
```

The harness creates only namespaced probe pods/Services/Jobs prefixed
`dksacc-<pid>` and deletes them on exit (`DKS_KEEP=1` to retain). It reads no
credentials beyond the kubeconfig kubectl already uses, and prints no secrets.
