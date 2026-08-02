# Peer-Hosted DKS — Appstore Apps (Helm charts)

Install, check, and remove an OSS appstore **app** — a Helm chart named by
`apps/<id>/app.yaml` (+ optional `apps/<id>/values.yaml`) — against a
**peer-hosted** DKS control plane, addressed purely by a kubeconfig path,
with **no cloudbox dependency anywhere on the execution path**.

- Packages: `internal/agent/appcatalog` (resolve/validate/render),
  `internal/agent/bundleapply` (apply/status/delete engine, reused as-is)
- Offline tests: `internal/agent/appcatalog/*_test.go`,
  `internal/agent/admincore/appstore_test.go`,
  `internal/agent/mcpapi/appstore_test.go`, `cmd/outpost/appstore_test.go`

This is the **apps** half of the peer parity surface against cloudbox's DKS
appstore. `docs/peer-dks-bundle-apply.md` covers the **builtins** half — an
operator-named built-in resolves to a raw `builtin/<name>/install.yaml`
manifest, applied byte-for-byte. An **app** instead resolves to a Helm chart
reference (`apps/<id>/app.yaml`) plus an optional values override
(`apps/<id>/values.yaml`), and is rendered into a single k3s
`helm.cattle.io/v1` `HelmChart` custom resource — never a raw manifest,
never a shelled-out Helm CLI invocation, never a templated string.

**Headline: this path is UNPROVEN on hardware**, for the same reason the
bundle-apply path is (see `docs/peer-dks-bundle-apply.md`): everything below
is validated only by deterministic, cluster-free tests against a fake
Kubernetes surface — including a fake simulating the k3s helm-controller's
`JobCreated` condition. No two-machine peer-hosted DKS run with a live
helm-controller has happened.

---

## Why a HelmChart CR instead of a Helm CLI

Two ways to make "install a Helm chart" work from this process were
considered:

1. A pinned, managed Helm CLI binary, shelled out to.
2. The k3s built-in `helm.cattle.io/v1` `HelmChart` custom resource, which
   the helm-controller shipped inside every k3s control plane (including a
   peer-hosted one — see `docs/cluster-peer.md`) reconciles into an
   in-cluster `helm upgrade --install`.

This package takes the second path. It needs no separate binary to
download, pin, or manage; it needs no chart-template execution or Helm SDK
in this process at all; and it composes for free with the existing
`bundleapply` engine — the rendered `HelmChart` object is just a
one-object `bundleapply.Bundle`, applied through the identical
`ApplyBundle`/`StatusBundle`/`DeleteBundle` functions the builtins path
uses, with the identical ordering, idempotent server-side apply, rollback,
and peer-venue-guard semantics. Every field on the object is set through
`unstructured.SetNestedField` on a typed Go value (`internal/agent/
appcatalog/render.go`); there is no string formatting, YAML templating, or
shell invocation anywhere on this path, so nothing from `app.yaml` or
`values.yaml` is ever interpolated into a command line. The k3s
helm-controller reconciling the resulting object is what actually executes
Helm, inside the cluster.

## The app.yaml / values.yaml schema

```yaml
# apps/<id>/app.yaml
schemaVersion: 1                 # the only version this package understands
id: cert-manager                 # must match the catalog directory name
displayName: cert-manager        # optional, operator-facing
license: Apache-2.0              # required; must be an allowlisted OSS SPDX id
platforms: [linux/amd64, linux/arm64]   # required; must include a k3s node platform
chart:
  repo: https://charts.jetstack.io      # required; https:// or oci:// only
  name: cert-manager                    # required
  version: v1.14.5                      # required — no floating "latest"
```

```yaml
# apps/<id>/values.yaml (optional — a chart with sane defaults needs none)
installCRDs: true
```

`values.yaml`'s bytes are carried **verbatim** into the rendered object's
`spec.valuesContent` — never parsed, templated, or otherwise interpreted by
this package.

### Fail-closed validation

Per `docs/fleet-evidence-invariant.md`, every unsupported dimension is a
hard failure, never a best-effort partial install:

- `schemaVersion` other than `1` (including missing/zero) — refused.
- `id` not present, or not equal to the catalog directory name — refused
  (defense in depth against a copy-pasted manifest under the wrong id).
- `license` missing or not on the allowlist (`internal/agent/appcatalog/
  manifest.go`'s `supportedLicenses` — the common permissive and copyleft
  OSS SPDX identifiers) — refused. This is the structural half of "no
  proprietary manifests": the catalog itself is a public, open-source
  checkout (same boundary `builtincatalog` enforces), and the license field
  is a second, explicit gate on top of that.
- `platforms` empty, or not containing at least one of `linux/amd64` /
  `linux/arm64` — refused. These are the only architectures a peer-hosted
  k3s control plane's compute half can ever be (a Linux container runtime,
  regardless of what OS/arch the **outpost daemon** administering the
  cluster happens to run on — a macOS outpost commonly administers a Linux
  k3s-agent container; see `internal/agent/runtime`). The check is therefore
  against this fixed set, never against the calling process's own
  `runtime.GOOS`/`GOARCH`.
- `chart.repo` not `https://` or `oci://`, or `chart.name`/`chart.version`
  empty — refused.
- A missing `apps/<id>/app.yaml`, or one that escapes the catalog root via
  symlink or traversal — refused, identically across install, status,
  *and* uninstall (no partial resolution, no cloudbox/private-manifest
  fallback), the same fail-closed contract `builtincatalog` established for
  the builtins path.

`outpost appstore show <id>` (`outpost_appstore_app` over MCP) runs this
exact validation **read-only, with no kubeconfig required**, so an operator
can see *why* an app is unsupported before naming a cluster at all.

## Per-user namespace / release isolation

`--namespace` is **required on every install/status/uninstall call** — there
is no default. Two different users (or an automation acting on their
behalf) can never collide on a silently-shared namespace. `--release`
(default: the app id) distinguishes multiple instances of the same app
within one namespace.

The rendered `HelmChart` object's name — and therefore the Helm release
name, since the k3s helm-controller runs `helm upgrade --install
<metadata.name> ...` — is the deterministic `<namespace>-<release>`
(`internal/agent/appcatalog/render.go`'s `Target.objectName`). Install,
status, and uninstall all **recompute** this name from the identical
`(namespace, release)` pair rather than looking anything up, so the three
operations can never diverge on which object they mean for "the same
install". Two users installing the same app id always render to two
distinct objects; one user installing the same app twice under distinct
`--release` values does too.

The `HelmChart` object itself always lives in the fixed `kube-system`
namespace (the same namespace k3s's own built-in `HelmChart` CRs, e.g.
Traefik, use) — this is what lets it be created before its own **target**
namespace exists. `spec.targetNamespace` names the caller's namespace and
`spec.createNamespace: true` is always set, so the helm-controller creates
it on first install. **Uninstall never deletes the target namespace** — it
may be hosting other apps for the same user; only the `HelmChart` CR (and
therefore the Helm release it drove) is removed.

## Readiness evidence — and its limit

`bundleapply`'s readiness evaluator (`internal/agent/bundleapply/
readiness.go`) gained a `HelmChart` case. The k3s helm-controller reports
two condition types on the object itself: `JobCreated` (the install/upgrade
Job was established) and `Failed` (the job hit an error under a
`FailurePolicy` of `abort`). **There is no third condition confirming the
underlying Job actually succeeded** — that evidence lives on the Job named
in `status.jobName`, which is outside the one-object bundle this package
applies and not tracked here.

`JobCreated=True` and not `Failed` is therefore the strongest per-object
evidence available, and is what "ready" means for a `HelmChart` object in
this package: it proves the controller accepted the object and is driving
the Helm install, **not** that the Helm run has completed. This mirrors the
same "no cluster is hardware-verified" caveat the rest of the peer-DKS
bundle-apply surface documents, rather than a silent, hidden guess.

## Go API

```go
entry, err := appcatalog.Resolve(catalog, "cert-manager")     // confined, symlink-safe
manifest, valuesYAML, err := appcatalog.Load(entry)            // schema/license/platform validated
target := appcatalog.Target{Namespace: "user-alice", Release: "cert-manager"}
obj, err := appcatalog.Render(manifest, valuesYAML, target)    // *unstructured.Unstructured, kind HelmChart

client, err := bundleapply.NewDynamicClient(kubeconfigPath)    // same venue-guarded constructor
res, err := bundleapply.ApplyBundle(ctx, &bundleapply.Bundle{Objects: []*unstructured.Unstructured{obj}},
    bundleapply.Options{Client: client, Timeout: 5 * time.Minute})
```

`appcatalog.ReleaseObject(target)` builds the same addressable
(kind/namespace/name) reference without needing the manifest at all — the
pure, deterministic name derivation `AppstoreStatus`/`AppstoreUninstall` use
so status/uninstall never need a live lookup to know what they're asking
about.

## Operator usage

```bash
# Preview a validated manifest — no cluster touched:
outpost appstore show cert-manager --catalog ../appstore

# List installable app ids:
outpost appstore list --catalog ../appstore

# Install into your own namespace:
outpost appstore install cert-manager --catalog ../appstore \
    --kubeconfig /path/k3s.yaml --namespace user-alice

# Check / remove the same install:
outpost appstore status cert-manager --catalog ../appstore --kubeconfig /path/k3s.yaml --namespace user-alice
outpost appstore uninstall cert-manager --catalog ../appstore --kubeconfig /path/k3s.yaml --namespace user-alice
```

`--offline` runs any of the above in the CLI process against the on-disk
`agent.json` — no running daemon required, mirroring the `bundle` subcommand
family. `cluster.bundle_catalog` / `cluster.bundle_kubeconfig` are shared
persisted defaults between the apps and builtins paths — one appstore
checkout normally ships both an `apps/` and a `builtin/` tree.

## Four-surface wiring

1. **File keys.** No new persisted keys — `cluster.bundle_catalog` and
   `cluster.bundle_kubeconfig` (see `docs/peer-dks-bundle-apply.md`) are
   reused as-is; a namespace/release is always an explicit per-call
   argument, never persisted (per-user isolation has no safe default).
2. **admincore (`internal/agent/admincore/appstore.go`).**
   `Server.AppstoreCatalog` / `AppstoreShow` / `AppstoreInstall` /
   `AppstoreStatus` / `AppstoreUninstall`. Install/status/uninstall each
   resolve+validate through `appcatalog`, render/recompute the one
   `HelmChart` object, then call `resolveBundleVenue` (the identical venue
   guard + client factory the builtins path uses) and
   `bundleapply.ApplyBundle` / `StatusBundle` / `DeleteBundle` directly —
   the same engine, not a reimplementation.
3. **MCP (`internal/agent/mcpapi/tools_appstore.go`).**
   `outpost_appstore_apps`, `outpost_appstore_app`,
   `outpost_install_appstore_app`, `outpost_appstore_app_status`,
   `outpost_uninstall_appstore_app`.
4. **CLI (`cmd/outpost/appstore.go`).** `outpost appstore
   {list,show,install,status,uninstall}`, each a thin MCP client with an
   `--offline` daemon-free path.

Parity is tested on every layer: `appcatalog/*_test.go` (confinement,
schema/license/platform fail-closed cases, structural rendering — including
a test proving shell-metacharacter-laden values survive as inert data, not
interpolated into anything), `bundleapply/readiness_test.go` (the
`HelmChart` case), `admincore/appstore_test.go` (behaviour, the venue guard,
and a two-user non-collision test using a fake `ResourceClient` that
simulates the helm-controller's `JobCreated` condition), `mcpapi/
appstore_test.go` (protocol roundtrip: install → status → uninstall →
status), and `cmd/outpost/appstore_test.go` (flag→params mapping, MCP
arg-key lockstep, and the offline path reaching the same admincore methods
with the same fail-closed venue/namespace refusals).

## Hardware status — UNPROVEN

Identical caveat to `docs/peer-dks-bundle-apply.md`: nothing here has run
against a live k3s helm-controller. Specifically unproven:

- A real `HelmChart` CR actually driving a real `helm upgrade --install`
  against a live peer-hosted k3s apiserver.
- Whether `JobCreated=True` is a tight enough readiness bar in practice, or
  whether a future revision needs to also read the named `status.jobName`
  Job for stronger completion evidence.
- Chart pull behavior against a real Helm repo (`spec.repo`) or OCI
  registry (`oci://`) from inside the cluster.

Standing up a peer-hosted control plane with the helm-controller enabled is
required to move any of the above from UNPROVEN to proven.
