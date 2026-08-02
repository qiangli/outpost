# Peer-Hosted DKS — Bundle / App Apply

Install, check, and remove an app/bundle manifest set against a
**peer-hosted** DKS control plane, addressed purely by a kubeconfig path,
with **no cloudbox dependency anywhere on the execution path**.

- Package: `internal/agent/bundleapply`
- Operator entry point: `script/dks-peer-bundle-apply.sh`
- Offline tests: `internal/agent/bundleapply/*_test.go`, `script/dks-peer-bundle-apply_test.sh`
- Run date of this write-up: 2026-08-01

**Headline: this path is UNPROVEN on hardware.** No two-machine run has happened.
Everything below is validated only by deterministic, cluster-free tests (fake
Kubernetes surface). See [Hardware status](#hardware-status--unproven).

---

## Why this exists

`docs/dks-peer-plane-gaps.md` is explicit that the peer plane is a **transport**
success but not yet a **product**: the machines connect, but *workloads* are the
gap. The existing app/bundle apply path assumes the **cloudbox-hosted** plane — a
cloud-minted kubeconfig plus a cloud-side orchestrator. A peer plane is just k3s
on someone's box: the operator has an admin kubeconfig and nothing else.

This package takes that kubeconfig **path** as a parameter and applies a bundle
using only `client-go`. There is no `access_token`, no `/api/v1` call, and no
overlay credential fetch on the apply path. If a future change finds itself
reaching for cloudbox on this path, that is a design regression — stop and write
down why instead (the whole point of the peer plane is that it stands alone).

## The two kubeconfigs are never conflated

```
peer control plane : ~/.kube/outpost-control-plane/k3s.yaml
cloudbox plane     : ~/.kube/outpost.yaml
```

Neither path is a default anywhere in the package — the caller always passes the
exact path. The named constants `bundleapply.PeerControlPlaneKubeconfig` /
`bundleapply.CloudboxKubeconfig` exist only so a caller can refer to a plane by
name instead of retyping a literal and accidentally landing the peer flow on the
cloudbox file. The operator script goes further: `--peer` resolves the peer path,
and any resolution that lands on the cloudbox kubeconfig is **refused** loudly
(`resolve_kubeconfig`), so the two planes can never be conflated through the
front door.

There is **no default cluster**. You pass `--peer` or `--kubeconfig PATH`
explicitly, or the run fails.

---

## Guarantees

1. **Ordering.** Objects are applied in a stable Helm-like install order
   (`internal/agent/bundleapply/order.go`). The hard requirement is **Namespace
   before any namespaced object**; the wider order puts CRDs, RBAC, config, and
   Services ahead of the workloads that consume them. Unknown kinds land in a
   conservative middle band (after config/RBAC, before workloads). Ties break
   deterministically by `(namespace, name, kind)` so two runs over one bundle
   produce the identical sequence.

2. **Idempotence.** Each object is applied with **server-side apply** under a
   stable field manager (`outpost-bundleapply`). Applying the same bundle twice
   converges on the same object set — it does not duplicate or error. (The
   deterministic test `TestApplyBundleIdempotent` asserts the store holds exactly
   one converged copy of each object after two runs.)

3. **Readiness.** An apply is **not done when the apiserver accepts the object** —
   it is done when the workload reports **Ready**. `WaitForReady` blocks up to an
   explicit, required timeout and returns a non-nil error on timeout, which the
   operator entry point turns into a **non-zero exit**. Readiness per kind:
   - Deployment / StatefulSet / ReplicaSet / ReplicationController:
     `status.observedGeneration >= metadata.generation` **and**
     `readyReplicas >= spec.replicas` (Deployment also requires `availableReplicas`).
   - DaemonSet: `numberReady >= desiredNumberScheduled` with **desired > 0**
     (a DaemonSet matching zero nodes is *not* proof of a running workload).
   - Pod: `Running` + `Ready` condition True, or `Succeeded`; `Failed` is terminal.
   - Job: `succeeded >= spec.completions`.
   - Kinds with no rollout concept (Namespace, ConfigMap, Secret, Service,
     ServiceAccount, RBAC, CRD, …) are Ready as soon as they exist — their
     creation *is* the evidence.

4. **Status and uninstall reuse the same evidence and ordering, not
   parallel logic.** `StatusBundle` (`internal/agent/bundleapply/status.go`)
   is a read-only pass that calls the identical `evalReadiness` function
   `WaitForReady` uses, so a status report's `ready` means exactly what a
   successful apply would have confirmed — never a second, drifting
   definition of "ready". `DeleteBundle` (`internal/agent/bundleapply/
   delete.go`) removes objects in reverse apply order through the same
   `deleteReverse` helper an apply failure's own rollback (`failAndCleanup`)
   uses — one delete failure does not stop the rest, and is reported
   precisely as an object the operator must remove by hand. An optional
   bounded `WaitForGone` confirms objects actually vanish rather than merely
   accepting the delete call.

## Evidence invariant

Per `docs/fleet-evidence-invariant.md`: **no success state may be reached by the
ABSENCE of evidence.** This package is default-deny on every unparseable or
absent signal:

- An empty bundle path, a directory with no manifests, or a comment-only file →
  `ErrEmptyBundle` (a hard failure — you pointed us at a path, something was
  expected there).
- A document missing `apiVersion` / `kind` / `metadata.name` → decode error, never
  silently dropped.
- An empty or missing kubeconfig → `ErrNoKubeconfig`. There is **no** fallback to
  an in-cluster config or a default cluster.
- A `Get` that fails during the readiness wait (apiserver unreachable, object
  vanished) → hard error, never treated as "ready".
- A workload that never becomes Ready → `ErrReadinessTimeout` → non-zero exit.
- An unsupported flag on the script → exit 2 with a loud message (coreutils/bashy
  convention; no silent fallback).
- A `Get` that fails during a status check, for any reason other than the
  object not existing (apiserver unreachable, permission error) → hard
  error, never reported as "not installed".
- A `Get` that fails during a post-uninstall gone-check, for any reason
  other than the object not existing → hard error, never treated as "gone"
  (`ErrDeletionTimeout` on a bounded-wait timeout instead).
- A built-in name that does not resolve to `builtin/<name>/install.yaml`
  under the catalog (missing, escapes the catalog root via symlink or
  traversal, or fails the operator-name pattern) fails closed on install,
  status, *and* uninstall alike — there is no partial resolution and no
  cloudbox/private-manifest fallback for any of the three operations. A
  chart or manifest format this package does not understand (arbitrary Helm
  charts, kustomize overlays, non-Kubernetes assets) is simply never
  resolved: the catalog only ever names plain `install.yaml` manifests, so
  an appstore entry that isn't one is invisible to `bundle catalog` and a
  direct name lookup fails the same way a missing built-in does — never a
  best-effort partial apply.

---

## Go API

```go
b, err := bundleapply.LoadBundle(bundlePath)          // file or dir of *.yaml/*.yml/*.json
client, err := bundleapply.NewDynamicClient(kubePath) // required path; no default cluster
res, err := bundleapply.ApplyBundle(ctx, b, bundleapply.Options{
    Client:       client,
    Timeout:      5 * time.Minute, // required, > 0
    PollInterval: 2 * time.Second,
    Log:          func(f string, a ...any) { /* stderr */ },
})
// res.Applied / res.Ready; err is non-nil (and the caller exits non-zero) on
// apply failure, readiness timeout, or a terminal workload failure.
```

`ResourceClient` is an interface (`Apply` / `Get`), so the apply and wait logic
is driven by a deterministic in-memory fake in tests — no apiserver, no
kubeconfig, no network. Production uses the `client-go` dynamic client plus a
discovery-backed REST mapper.

## Operator usage

```bash
# Peer control plane (resolves ~/.kube/outpost-control-plane/k3s.yaml):
script/dks-peer-bundle-apply.sh --peer --bundle ./bundle/

# Explicit kubeconfig:
script/dks-peer-bundle-apply.sh --kubeconfig /path/k3s.yaml --bundle ./app.yaml \
    --timeout 5m --poll 2s

# Install by OSS appstore built-in name through the normal CLI/MCP surface:
outpost bundle install headlamp --catalog ../appstore --kubeconfig /path/k3s.yaml
outpost bundle catalog

# Discover, then check and remove the same built-in:
outpost bundle catalog --catalog ../appstore
outpost bundle status headlamp --catalog ../appstore --kubeconfig /path/k3s.yaml
outpost bundle uninstall headlamp --catalog ../appstore --kubeconfig /path/k3s.yaml
```

The named install path accepts only `builtin/<name>/install.yaml` beneath a
canonicalized open-source appstore root. Traversal, escaping symlinks, missing
assets, and unavailable catalogs fail closed. The source can be the umbrella's
sibling `appstore` checkout or a separately fetched/versioned appstore tree;
there is no cloudbox/private-manifest fallback. Once resolved, the manifest is
sent through the same readiness, rollback, and peer-venue guard as an explicit
bundle apply.

`bundle status <name>` resolves the identical manifest `bundle install` would
apply and reports each object's live state — `exists`, `ready`, a short
reason — using the same readiness evidence (`evalReadiness`) the apply's
rolled-out wait uses. It is a pure read: nothing is applied, deleted, or
waited on. `bundle uninstall <name>` resolves the same manifest and removes
every object in reverse apply order, reusing the identical best-effort
deletion mechanics an apply failure's own rollback already relies on — one
delete failure does not stop the rest, and is reported precisely as an object
left behind for the operator to remove by hand. Both reuse `BundleApply`'s
venue resolution (`resolveBundleVenue`): explicit `--kubeconfig` > persisted
`cluster.bundle_kubeconfig` > the conventional peer path, with the cloudbox
kubeconfig always refused. This is the peer-DKS parity surface for the
install/status/uninstall lifecycle cloudbox's own DKS catalog already offers —
scoped to public, open-source appstore entries only; there is no support here
for arbitrary Helm charts or private cloudbox manifests, and an unresolvable
or unsupported name fails closed rather than guessing.

The script owns argument handling, the peer/cloudbox distinction, and loud
failure on anything unsupported or absent; all Kubernetes work lives in the Go
package (`go run ./internal/agent/bundleapply/cmd`).

---

## Tests — deterministic and cluster-free

```bash
go build ./...
go test -short ./internal/agent/bundleapply/...
bash script/dks-peer-bundle-apply_test.sh
```

- The Go tests fake the Kubernetes surface entirely (`fake_test.go`) — apply
  ordering, idempotent convergence, readiness across polls, terminal-pod
  fast-fail, unreachable-apiserver failure, and bad-option rejection are all
  observed without a cluster. `status_test.go` and `delete_test.go` cover the
  status/uninstall additions the same way: not-installed vs. installed-and-
  ready reporting, zero-desired-is-not-fatal, reverse-order deletion,
  partial-delete-failure accounting, and the bounded gone-check's timeout and
  hard-error paths.
- The script test executes the **real** script against a **stub** runner on a
  minimal env and asserts exit status, messages, and the exact args the stub
  received — nothing simulates the script's logic by hand.

No cluster, kubectl, or network is required for any of this.

---

## Hardware status — UNPROVEN

**Nothing here is hardware-verified.** No two-machine peer-hosted DKS run has
occurred. Specifically unproven:

- Applying a real bundle against a live `~/.kube/outpost-control-plane/k3s.yaml`
  (k3s client-cert auth path through `client-go`).
- Discovery / REST-mapping against a real k3s apiserver (CRDs, API groups).
- Real rollout readiness timing (image pulls, scheduling latency) versus the
  bounded wait.
- Cross-node scheduling on a tunnelled-worker topology.

The only cluster reachable during prior peer-DKS runs was the **cloudbox-hosted**
overlay (see `docs/peer-dks-hardware-acceptance.md`), which is the wrong venue for
this path by construction. Standing up a peer-hosted control plane with a
tunnelled worker is required to move any of the above from UNPROVEN to proven.

Hosts above are named by **role** (control-plane host, tunnelled worker), never by
real hostname.

---

## Four-surface wiring

The package is wired into the normal outpost surfaces, in the repo convention
(land in `admincore` first, wrappers in lockstep). Every surface converges on
the same bundle transaction — `admincore.Server.BundleApply` — so the venue
guard, rolled-out readiness bar, and rollback accounting cannot drift:

1. **File key (`internal/agent/conf/file.go`).** `cluster.bundle_kubeconfig` —
   the persisted **default venue** used when an apply doesn't pass a kubeconfig
   explicitly. Side-effect class: **Live** (read on each apply, never restarts
   the daemon). It is a fallback, not a bypass: whatever source supplies the
   path — explicit arg, persisted key, or the conventional peer default — it
   goes through `bundleapply.ResolveVenue` and the cloudbox kubeconfig is
   refused after canonicalization.
   `cluster.bundle_catalog` is the optional persisted OSS appstore root for
   named installs. It is also Live and can name a fetched/versioned tree.

2. **admincore (`internal/agent/admincore/bundle.go`, `builtin.go`).**
   `Server.resolveBundleVenue(explicit)` is the shared venue-resolution +
   client-construction helper: explicit param > persisted
   `cluster.bundle_kubeconfig` > conventional peer path, guard enforced **in
   the Go API**, then the client is built through `Deps.BundleApplyClient`
   (the test seam — it only ever sees an accepted, canonicalized path, so an
   injected client cannot bypass the guard). `Server.BundleApply` uses it and
   runs `LoadBundle` + `ApplyBundle`, returning the transactional accounting
   (`Created` / `RolledBack` / `CleanupFailed`). `Server.BundleStatus` uses
   the same helper and runs `LoadBundle` + `bundleapply.StatusBundle` — a
   read-only pass reusing the identical `evalReadiness` evidence the apply
   wait uses. `Server.BundleUninstall` uses the same helper and runs
   `LoadBundle` + `bundleapply.DeleteBundle` — the identical reverse-order,
   best-effort deletion mechanics `ApplyBundle`'s own rollback path uses.
   `Server.BundleKubeconfig()` reports the persisted venue.
   `Server.effectiveCatalog(explicit)` is the equivalent shared helper for
   catalog resolution (explicit > persisted `cluster.bundle_catalog`).
   `Server.BuiltinInstall` / `BuiltinStatus` / `BuiltinUninstall` each resolve
   an OSS built-in name through it and delegate to the matching `Bundle*`
   method, so a built-in's install/status/uninstall lifecycle always resolves
   the identical manifest; `Server.BundleCatalog` reports the effective
   source and installable names.

3. **MCP (`internal/agent/mcpapi/tools_bundle.go`).** `outpost_apply_bundle`
   (args `kubeconfig?`, `bundle`, `timeout_seconds?`, `poll_seconds?`,
   `crd_timeout_seconds?`, `allow_scale_to_zero?`, `no_rollback?`,
   `save_kubeconfig?`), `outpost_bundle_kubeconfig`,
   `outpost_install_builtin`, `outpost_builtin_status`,
   `outpost_uninstall_builtin`, and `outpost_bundle_catalog`. Thin wrappers;
   `APIError` maps to `CallToolResult.IsError`.

4. **CLI (`cmd/outpost/bundle.go`).** `outpost bundle apply <file-or-dir>
   [--kubeconfig PATH] [--timeout N] [--poll N] [--crd-timeout N]
   [--allow-scale-to-zero] [--no-rollback] [--save-kubeconfig] [--offline]`
   plus `outpost bundle install <name>`, `outpost bundle status <name>`,
   `outpost bundle uninstall <name>`, `outpost bundle catalog`, and
   `outpost bundle kubeconfig`. The default path is a thin MCP client;
   `--offline` calls the same admincore method in-process (no daemon). The
   standalone `script/dks-peer-bundle-apply.sh` remains the shell-only path.

5. **Docs.** The settings inventory (`docs/settings.md`, "Applying bundles to a
   peer-hosted plane") documents the key + tools + flags; the embedded copy is
   kept byte-identical (`cmd/outpost/docs_test.go` enforces).

Parity is tested on all three code surfaces: `admincore/bundle_test.go` +
`builtin_test.go` (behaviour + venue guard, including status/uninstall),
`mcpapi/bundle_test.go` (protocol roundtrip landing on `BundleApply` /
`BuiltinStatus` / `BuiltinUninstall`), and `cmd/outpost/bundle_test.go`
(flag→params mapping, MCP arg-key lockstep, and the offline path reaching the
same method with the same 400 venue refusal).
