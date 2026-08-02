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
- Harness corrective: sprint 42 (see *Harness corrective* below). The live run
  recorded under *Results* was produced by the **pre-corrective** harness; that
  section is preserved as the historical record and is annotated where the
  corrected harness would score the same observations differently.

---

## Harness corrective (sprint 42)

The pre-corrective harness could report success without the underlying fact
being true. Five paths were closed; a gate that can go green while blind is
worse than no gate, because it launders absence of evidence into a claim of
proof.

1. **A trivially-true precondition could saturate the whole gate.** The exit
   contract was "≥1 PASS and 0 FAIL", and `nodes-ready` passes on any two-node
   cluster. A run with all ten substantive checks BLOCKED printed `RESULT OK`
   and exited `0`. Checks are now split into **preconditions** (`preflight`,
   `nodes-ready`, `peer-venue`, `distinct-pod-cidrs`) and **substantive**
   checks; exit `0` requires at least one *substantive* check — one that
   actually moved a packet or inspected a host, not one that only read the
   API server — to have been OBSERVED to pass. See *Check taxonomy*.
2. **`kubectl exec`'s return code was never read.** `service-clusterip` scored
   on a negative blacklist — PASS unless the output mentioned
   `refused|timed out|no route|error` — so busybox's `Network is unreachable`
   scored PASS, with the error text quoted in the PASS line. A negative
   blacklist can only enumerate the failures someone thought of and treats every
   unanticipated failure as success. It is replaced by a **positive content
   assertion** (the request must return the backend pod's own
   `/etc/hostname`, i.e. bytes only the pod on the other node could produce)
   **plus** the exec's return code. Every other exec-based check
   (`cross-node-pod-ip`, `cluster-dns`, `logs-exec`) was audited and now reads
   its return code as well; `headlamp`'s documented exception is below.
3. **Nothing asserted the venue.** The harness would emit a green verdict
   against *any* two-node cluster, including the cloudbox-hosted one that runs
   no flannel at all. The new `peer-venue` check positively asserts the ADR
   Option A topology (every Ready kubelet-backed node carries a
   `flannel.alpha.coreos.com/public-ip` annotation inside the Tailscale CGNAT
   range 100.64/10). A cluster observed *not* to be that **FAILs** — this is the
   peer-DKS gate, and results gathered somewhere else are not results about it.
4. **Operator-supplied evidence was indistinguishable from observed evidence.**
   `DKS_HOST_EVIDENCE` is a text file a human typed; it could produce a full
   `flannel-iface` PASS with no machine inspected, and the PASS record carried no
   provenance marker. The two kinds are now structurally distinct. See
   *Evidence provenance*.

Also corrected, from the same gate review:

- a missing `app.kubernetes.io/name=headlamp` label now yields **BLOCKED**, not
  FAIL — an unlabelled deployment is a backend this harness cannot *see*, which
  is missing evidence, not a defect of the pod network;
- `bashy-chunked` gained the `ImagePullBackOff`/`ErrImagePull` → **BLOCKED**
  carve-out that `nanochat` already had — an image that never became pullable
  means the distributed workload never ran;
- a one-node venue is now **BLOCKED** (→ INCONCLUSIVE), not FAIL — an absent
  venue proves nothing either way, and calling it a defect both inverts the
  harness's doctrine and hides the real reason the run was worthless.

**Nothing in this corrective changes what has been proven on hardware. It has
still never run on two peer-hosted machines.** The corrective makes the harness
*able* to tell you that, rather than able to say `OK` while blind.

---

## Check taxonomy

Checks are classified by a single test: *if the cluster were completely broken — no
CNI, no routes, not one packet able to cross between nodes — but the API server
were healthy, could this check still PASS?* If yes, it is a precondition. If not,
it is substantive evidence — and only because it moved a packet or inspected a host.

| Class | Checks | Meaning |
| --- | --- | --- |
| **Precondition** | `preflight`, `nodes-ready`, `peer-venue`, `distinct-pod-cidrs` | Describe the venue, not the slice. Passing says nothing about whether peer-hosted pod networking works. |
| **Substantive** | `flannel-iface`, `no-stale-conflist`, `cross-node-pod-ip`, `service-clusterip`, `cluster-dns`, `logs-exec`, `headlamp`, `nanochat`, `bashy-chunked` | Assert a property of the slice under test. Each moved a packet across the pod network or inspected a host. Only these can make a run green. |

A run in which no substantive check was observed to pass is `INCONCLUSIVE`
(exit `2`) even if every precondition passed. The classification is enforced by
**allowlist** (not blacklist): a check not explicitly listed as substantive
contributes nothing to a green verdict, so a future API-only check cannot inherit
the saturating role by being forgotten.

`distinct-pod-cidrs` was the worked example of the rule: it reads `.spec.podCIDR`
from the API server and nothing else — no probe pod, no host inspection, no
packet. Distinct per-node podCIDRs are IPAM bookkeeping that stays true whether
or not flannel runs, tailscale0 exists, or a byte can cross between nodes. For
five sprints it was the only substantive check that survived a degraded
environment, which made it precisely the check that passed when everything else
blocked — the successor saturator that closed the first four false-greens and
created the fifth. It is still run and reported (a duplicate/absent podCIDR is a
real defect), but its PASS no longer makes a run green.

---

## Evidence provenance

Two kinds of evidence reach the harness, and they are kept structurally
distinct in the `CHECK` line, in the `SUMMARY` tally, and in the exit code:

| Kind | Source | Marker |
| --- | --- | --- |
| **OBSERVED** | Read by the harness from the API server, or from a host-inspection pod it created and whose logs it collected (`DKS_ALLOW_NODE_DEBUG=1`). | each item tagged `(observed)`; a check that rests entirely on observed items records `PASS` |
| **ATTESTED** | Read out of the operator-authored `DKS_HOST_EVIDENCE` file. No machine was inspected; a human typed it. | each item tagged `(attested)`; the check records the distinct status `ATTESTED`, tallied on its own counter |

**Decision: operator-attested evidence may NOT contribute to a green verdict.**
It is still accepted and reported — an operator's collected evidence is genuinely
useful for triage on a host where inspection RBAC is unavailable, and reading it
is how a contradiction gets surfaced at all — but a check whose only path to
passing runs through attested items records `ATTESTED`, never `PASS`, and the run
exits `2` unless something else was actually observed. A single attested item
demotes the whole record: mixed provenance is attested provenance.

An attested **contradiction** still FAILs. Declining to trust an attestation as
proof of health is not a reason to ignore an operator reporting a defect.

`ATTESTED` is neither a pass nor a failure, and can never be rendered as `PASS`
— the status token itself differs, so a grep for `PASS` cannot match it.

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
| outpost | present, on PATH |
| podman | present; docker absent; **tailscale CLI absent on this host** |
| kubeconfig contexts | `cloudbox` (current), peer-cluster-context |
| current context | `cloudbox`, namespace `test-namespace` |
| local outpost identity | `agent_name` present; `has_token: false` |
| local outpost cloudbox | `https://ai.dhnt.io`, protocol `wss` |
| RBAC in namespace | `create pods` = yes, `get nodes` = yes |
| Two machines available? | **Yes — but not of the right kind** (see below) |

Nodes visible via the `cloudbox` context (`kubectl get nodes -o wide`):

| Node | Kind | Status | podCIDR |
| --- | --- | --- | --- |
| `worker-a` | k3s agent (`v1.36.1+k3s1`, arm64) | Ready | `10.42.0.0/24` |
| `worker-b` | k3s agent (`v1.36.1+k3s1`, amd64) | Ready | `10.42.2.0/24` |
| `stale-worker-a` | k3s agent | NotReady | `10.42.1.0/24` |
| `stale-worker-b` | k3s agent | NotReady, SchedulingDisabled | `10.42.5.0/24` |
| 13 × `virtual-worker-*-{native,podman,ollama}` | virtual-kubelet (`v0.1.0-vknode`) | mixed | mixed / `<none>` |

**Decisive venue finding.** Two Ready kubelet-backed nodes *do* exist
(`worker-a`, `worker-b`), so the multi-node checks could run. But
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

**These are the pre-corrective harness's verdicts, preserved verbatim.** Under
the corrected harness the same observations would additionally produce
`CHECK peer-venue FAIL` (no node in this cluster carries a flannel public-ip
annotation, so it is demonstrably not the peer-hosted plane this gate exists to
accept), and #8's PASS would not be captioned as being about a "remote/tunnelled"
node. The overall verdict is unchanged: `RESULT FAIL`, exit `1`. Nothing was
re-run on hardware for the corrective.

| # | Check | Result | Evidence / missing precondition |
| --- | --- | --- | --- |
| 1 | `nodes-ready` | **PASS** | `2 Ready kubelet-backed nodes: worker-a worker-b` |
| 2 | `distinct-pod-cidrs` | **PASS** | `2 distinct podCIDRs: 10.42.0.0/24 10.42.2.0/24` (Ready kubelet-backed nodes only) |
| 3 | `flannel-iface` | **BLOCKED** | `no node carries annotation flannel.alpha.coreos.com/public-ip; flannel is not running on any node (peer-flannel path not deployed here)` |
| 4 | `no-stale-conflist` | **BLOCKED** | `missing host evidence on nodes; set DKS_ALLOW_NODE_DEBUG=1 to permit host-inspection pods (hostPID + hostNetwork + read-only /host mount; requires pod execution RBAC), or supply DKS_HOST_EVIDENCE=<file>` |
| 5 | `cross-node-pod-ip` | **FAIL** | `dksacc-pid-a@worker-a -> 10.42.2.17@worker-b: 3 packets transmitted, 0 packets received, 100% packet loss` |
| 6 | `service-clusterip` | **FAIL** | `dksacc-pid-a -> clusterIP 10.43.25.178:8080: wget: download timed out` |
| 7 | `cluster-dns` | **FAIL** (flaky) | `** server can't find dksacc-svc.svc.cluster.local: NXDOMAIN` — see note below |
| 8 | `logs-exec` | **PASS** | `logs+exec ok against dksacc-pid-b@worker-b` (the second selected node — see note) |
| 9 | `headlamp` | **PASS** | `Headlamp headlamp/headlamp at 10.43.246.3:80 reachable (HTTP/1.1 404 from /)` |
| 10 | `nanochat` | **BLOCKED** | `DKS_NANOCHAT_IMAGE unset; no nanochat image is published in this repo and this harness will not substitute a weaker workload` |
| 11 | `bashy-chunked` | **BLOCKED** | `DKS_BASHY_IMAGE unset; no chunked-bashy container image or Job manifest exists in this repo, and there is no deterministic substitute for a distributed chunked workload` |

### Notes on individual results

- **#5 `cross-node-pod-ip` — reproduced 3/3 runs.** A busybox pod pinned to
  `worker-a` cannot reach a pod IP on `worker-b` at all. This is a
  genuine, repeated hardware failure of the *cloudbox-hosted* overlay
  (outpost-cni + advertised routes), observed on 2026-08-01. It says nothing
  about the peer-flannel path, which is not deployed here.
- **#6 `service-clusterip`** follows directly from #5 — the Service's only
  backend pod is on the remote node, so the timeout is the same underlying
  failure, not an independent one.
- **#7 `cluster-dns` is FLAKY across runs, not stably failing.** It PASSed on
  two runs (short name resolved to stable CIDR) and NXDOMAIN'd on the final run.
  The most likely cause is a CoreDNS propagation race against a Service created
  seconds earlier, not a cross-node DNS defect. Recorded as FAIL because that is
  what the final run actually produced; it should not be read as a settled result.
- **#8 `logs-exec` — the earlier caption calling `worker-b` "the
  remote/tunnelled node" was an overreach and has been removed.** The harness
  verifies that `kubectl logs` and `kubectl exec` work against a pod on the
  second selected node. It does **not** verify how the API server reaches that
  node's kubelet, so it cannot claim the path was remote or tunnelled. That was
  an assumption about the venue presented as an observation.
- **#9 `headlamp`** returned HTTP 404 from `/`. The check asserts *Service
  reachability*, and any well-formed HTTP status line proves that; the path
  Headlamp serves its UI on is not a pod-network property. Note Headlamp pods
  run on **both** Ready nodes, so this reachability may have been
  satisfied by the node-local replica and is **not** independent proof of
  cross-node Service routing.
- **#2 `distinct-pod-cidrs`** is scoped to **Ready** kubelet-backed nodes only.
  Stale NotReady nodes and virtual-kubelet nodes are excluded: stale nodes do
  not participate in active scheduling, and virtual-kubelet nodes do not
  participate in pod networking at all. Six virtual nodes carry no `podCIDR`;
  one stale real node shares a CIDR with a virtual node. See *Defects found*.

---

## Code-testable automation vs external hardware blockers

### Proven offline (no cluster required)

`bash script/dks-peer-acceptance_test.sh` → **302 pass, 0 fail** (pure unit tests;
runner/integration tests execute end-to-end with a stub kubectl). It asserts the
harness's own logic, including the mandatory invariants this story turns on.

The tests marked `FALSE-GREEN` in that file each reproduce a way the
pre-corrective harness reported success (or a wrong verdict) without the
underlying fact being true; every one of them fails against the pre-corrective
runner. The suite honours `DKS_HARNESS=<path>` so that claim is checkable —
and it was checked, not merely asserted:

```bash
git show 42f61b5:script/dks-peer-acceptance.sh >/tmp/old.sh
DKS_HARNESS=/tmp/old.sh bash script/dks-peer-acceptance_test.sh
# → TESTS pass=214 fail=88   (exit 1)
```

The 88 failures are the four false-greens and the three doctrine inversions,
each named in the failing test's label. Against the current harness the same
suite is `pass=302 fail=0`.

The suite installs stand-ins for the helpers a pre-corrective harness does not
have (`dks_is_precondition`, `dks_resolve_ev`, and the `attested` /
`substantive_pass` counters). Without them the run aborts under `set -u` at the
first reference and demonstrates nothing about the tests after it — a
verification command that crashes is not a verification. Each stand-in returns
the answer a harness lacking that machinery deserves, so the dependent
assertion FAILs and the run continues; the shim is skipped entirely when the
real definition is present, so it cannot mask a regression in the current
harness.

The headline false-green, reproduced end to end against a stub cluster on which
every substantive check is BLOCKED — same observations, opposite verdicts:

```
# pre-corrective
SUMMARY pass=1 fail=0 blocked=4
RESULT OK (no failures; 4 blocked)                                   → exit 0

# corrected
SUMMARY pass=1 fail=0 blocked=4 attested=0 substantive_pass=0
RESULT INCONCLUSIVE (only precondition checks passed; no substantive
                     check proved anything)                          → exit 2
```

Asserted invariants:

- a `BLOCKED` check never tallies as, or renders as, `PASS` — mandatory invariant;
- an `ATTESTED` check likewise never tallies as, or renders as, `PASS`;
- the three exit codes, asserted against the **real runner process** (not its
  stdout): `0` only with ≥1 **substantive** observed PASS and 0 FAIL, `1` on any
  FAIL, `2` when nothing was proven — see *Exit codes* below;
- all-blocked reports `INCONCLUSIVE`, never `OK`, and exits `2` — a skipped-only
  run cannot read as green to a gate that reads only `$?`;
- **NEW (sprint 42)**: a run in which the preconditions passed and every
  substantive check was BLOCKED exits `2` with
  `RESULT INCONCLUSIVE (only precondition checks passed…)`. This is the
  false-green the ≥1-PASS gate allowed: `nodes-ready` alone satisfied it;
- **NEW (sprint 42)**: `peer-venue` PASSes only when every Ready kubelet-backed
  node carries a tailnet-range flannel public-ip annotation; a cluster with no
  flannel, with a non-tailnet underlay, or only partially annotated **FAILs**;
  an unanswerable query BLOCKs. A `peer-venue` PASS on its own is still
  `INCONCLUSIVE` — it is a precondition;
- **NEW (sprint 42)**: every exec-based check reads `kubectl exec`'s return
  code, and asserts positively on the expected payload. Fixtures cover the
  combinations that a negative blacklist confuses: rc≠0 with success-shaped
  text (FAIL), rc 0 with the wrong payload (FAIL), rc 0 with the right payload
  (PASS), and rc 0 with an empty body (FAIL);
- **NEW (sprint 42)**: an otherwise-passing `flannel-iface` /
  `no-stale-conflist` resting on `DKS_HOST_EVIDENCE` records `ATTESTED` and
  exits `2`; mixed observed/attested provenance also records `ATTESTED` and
  names which node was not inspected; an attested *contradiction* still FAILs;
- **NEW (sprint 42)**: a missing `app.kubernetes.io/name=headlamp` label, a
  non-Running Headlamp pod, and a Service with no clusterIP each BLOCK (naming
  the missing precondition) instead of FAILing; a Running, addressable Headlamp
  that returns no HTTP status line still FAILs;
- **NEW (sprint 42)**: `bashy-chunked` BLOCKs on `ImagePullBackOff`/
  `ErrImagePull` and still FAILs on a genuine incomplete-job result;
- **NEW (sprint 42)**: a one-node venue BLOCKs rather than FAILs;
- **NEW (sprint 42)**: a grep guard so the negative error blacklist cannot come
  back;
- a `DKS_ONLY` naming no existing check runs nothing and exits `2`, not `0`;
- distinct-CIDR comparison: duplicate, empty, missing-field, late-duplicate and
  zero-node inputs all return failure with the offending node named;
- **NEW**: annotation-only (flannel-iface) never PASS — host inspection (hostPID +
  hostNetwork + read-only /host mount) or operator evidence required (regression
  guard: checks missing host evidence return BLOCKED, not PASS);
- **NEW**: cleanup pod tracking — inspection pods use deterministic `${RUN_ID}-insp-<slug>-<idx>`
  naming convention and are tracked in CREATED array for cleanup (no leaked
  inspection pods);
- **NEW**: host-evidence vocabulary shared between inspection-pod output and
  `DKS_HOST_EVIDENCE` file (k3s_argv, tailscale0, cni_confdir), fallback when
  RBAC unavailable;
- **NEW**: stale-node exclusion — distinct-pod-cidrs scoped to Ready nodes only;
  mixed input filters leave Ready-only result;
- **NEW**: flannel-iface requires per-node equality between the
  `flannel.alpha.coreos.com/public-ip` annotation and the observed
  `tailscale0` IPv4 on that SAME node — an annotation and interface that are
  each individually well-formed but name *different* tailnet addresses is an
  observed contradiction (`FAIL`), never a `PASS`; missing evidence on either
  side stays `BLOCKED` (regression guard: `ev-good`/`ev-good-b`/`ev-mismatch-b`
  fixtures cover the equality-PASS and mismatch-FAIL cases explicitly);
- **NEW**: `service-clusterip` / `cluster-dns` gate on the source probe pod
  (`POD_A`) having become Ready (`ERR_A`), not just on the backend Service
  having a clusterIP — a not-Ready `POD_A` makes `kubectl exec` itself fail,
  which previously satisfied the FAIL heuristic and reported a false `FAIL`
  instead of `BLOCKED` for a precondition that was never met;
- **NEW**: `nanochat` waits on `kubectl rollout status --timeout="$DKS_TIMEOUT"s`
  instead of a fixed `sleep 10`, so a slow-but-eventually-successful pull is not
  penalized and a genuinely stuck rollout is not given less time than the
  operator configured. A rollout that never completes because the image was
  never pullable (`ImagePullBackOff`/`ErrImagePull`) is a missing precondition
  (`BLOCKED`, naming the image), distinct from a real cross-node placement
  defect (`FAIL`);
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
| No peer-hosted DKS cluster reachable | `peer-venue`, #3, #4, and the *meaning* of #5–#9 | Two machines joined to a **peer-hosted** control plane (`outpost cluster control-plane on` + `cluster join`), each with a Headscale-issued tailnet identity. Out of scope: provisioning/pairing was explicitly forbidden. |
| No host-inspection RBAC or operator evidence | #4 | Run with `DKS_ALLOW_NODE_DEBUG=1` to permit host-inspection pods (hostPID + hostNetwork + read-only / mount at /host; requires pod execution RBAC), or collect host evidence manually and pass `DKS_HOST_EVIDENCE=<file>` (format: one node per line, `<node> <key>=<value> [<key>=<value> ...]`) — which yields `ATTESTED`, not `PASS`, and so does not close the blocker. |
| No published nanochat image | #10 | A container image reference in `DKS_NANOCHAT_IMAGE`. None exists in this repo. |
| No chunked-bashy image or Job manifest | #11 | A container image reference in `DKS_BASHY_IMAGE`. None exists in this repo. |

**What would close this out:** run the same harness unchanged against a
peer-hosted cluster with **`DKS_ALLOW_NODE_DEBUG=1`** (host-inspection pods),
plus the two image env vars. No harness change is required — `flannel-iface`
and `no-stale-conflist` are implemented and will assert, not skip, the moment a
flannel-bearing node is present, and `peer-venue` will confirm the cluster is
the peer-hosted one rather than leaving that to the reader's assumption.

`DKS_HOST_EVIDENCE=<file>` is **not** a substitute for that run: operator
attestation produces `ATTESTED`, never `PASS`, so a host-evidence-only run
cannot close this out (it exits `2`). Use it for triage on a host where pod
execution RBAC is unavailable — it will still surface a contradiction as a
FAIL — but the gate closes on observation.

---

## Defects found

Not fixed here — this story is forbidden from touching production code.

1. **Duplicate `podCIDR` across a real and a virtual node.**
   One stale real node and one virtual-kubelet node both hold the same CIDR.
   Whether the k3s controller-manager allocator and the vknode node registration
   draw from the same pool is worth confirming: if a virtual node can consume a
   CIDR the real allocator later hands out, the B5 distinctness property is not
   actually guaranteed cluster-wide. Registration path: `internal/agent/vknode/node.go`.
2. **Six virtual-kubelet nodes carry no `.spec.podCIDR`**. Benign if virtual
   nodes never route pods, but it means any naive cluster-wide `podCIDR`
   distinctness assertion fails; the harness scopes around it deliberately.
3. **`cross-node-pod-ip` fails on the cloudbox-hosted overlay** (100% loss,
   3/3 runs, worker-a → worker-b). This is the *existing*
   outpost-cni + advertised-routes path, i.e. Option B in
   `docs/adr-peer-dks-pod-network.md`. It is corroborating evidence for the ADR's
   choice of Option A, and a live defect in the current overlay in its own right.

---

## Reproducing

```bash
# Offline — no cluster needed.
bash script/dks-peer-acceptance_test.sh

# Full acceptance against a peer-hosted cluster (with host inspection).
KUBECONFIG=~/.kube/peer.yaml \
DKS_ALLOW_NODE_DEBUG=1 \
DKS_NANOCHAT_IMAGE=<image> \
DKS_BASHY_IMAGE=<image> \
  bash script/dks-peer-acceptance.sh

# Triage with operator-collected host evidence (no pod RBAC needed).
# NOTE: host-evidence items are ATTESTED, not observed — the host-evidence
# checks report ATTESTED rather than PASS and the run exits 2. This mode
# surfaces contradictions; it cannot produce a green verdict.
KUBECONFIG=~/.kube/peer.yaml \
DKS_HOST_EVIDENCE=host-evidence.txt \
DKS_NANOCHAT_IMAGE=<image> \
DKS_BASHY_IMAGE=<image> \
  bash script/dks-peer-acceptance.sh

# A single check.
DKS_ONLY=cross-node-pod-ip bash script/dks-peer-acceptance.sh
```

### Exit codes

A gate may read the exit status alone; it carries the full verdict.

| Code | RESULT line    | Meaning                                                     |
|------|----------------|-------------------------------------------------------------|
| `0`  | `OK`           | At least one **substantive** check was **observed** to PASS, and nothing FAILed. |
| `1`  | `FAIL`         | At least one check FAILed. Outranks INCONCLUSIVE.            |
| `2`  | `INCONCLUSIVE` | Nothing was proven: no substantive check was observed to pass. |

Exit `2` covers four distinct situations, named on the `RESULT` line so an
operator knows what to do next:

| `RESULT INCONCLUSIVE (…)` | Situation |
| --- | --- |
| `no check ran` | e.g. a `DKS_ONLY` name that matches nothing. |
| `no check passed` | every check that ran was BLOCKED. |
| `only precondition checks passed; no substantive check proved anything` | a venue exists, but nothing about the slice under test was established. |
| `only operator-attested evidence; no substantive check was observed to pass` | the only thing that would have passed rests on a human-authored evidence file. |

`2` is deliberately non-zero in all four. Nothing being proven is not the same
as nothing being wrong; scoring it as success would be exactly the
absence-of-evidence-as-success failure `docs/fleet-evidence-invariant.md`
forbids. `script/dks-peer-acceptance_test.sh` asserts every code — and every
one of the four `INCONCLUSIVE` wordings — against the real runner process
(offline, with a stub `kubectl`).

The `SUMMARY` line carries the full tally, including the two counters the exit
code turns on:

```
SUMMARY pass=<n> fail=<n> blocked=<n> attested=<n> substantive_pass=<n>
```

The harness creates only namespaced probe pods/Services/Jobs prefixed
`dksacc-<pid>` and deletes them on exit (`DKS_KEEP=1` to retain). It reads no
credentials beyond the kubeconfig kubectl already uses, and prints no secrets.
