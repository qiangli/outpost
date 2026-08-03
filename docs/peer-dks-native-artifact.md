# Peer-DKS native artifacts — whole-tree, content-addressed, one trust path

`vknode`'s native-process backend can realize a Pod as a host process whose
executable is not on `$PATH` but delivered as a **release artifact**: an archive
named by a URL, pinned by a sha256, downloaded → verified → cached → run. This
is the mechanism behind peer-DKS "native" runners (llama.cpp, an ollama server,
a Python runner, …) that need direct host/hardware access a container cannot
give.

Until sprint 42 the extractor pulled **exactly one** archive member to a single
file. That is enough for a self-contained binary, but not for an artifact that
ships a runner **plus the sibling files it needs at launch** — the nanochat
2-node design (dhnt#4) ships `bin/<runner>` together with `pyproject.toml` +
`uv.lock` in one archive and builds a hermetic venv from that lock, so those
files must never be resolved from, or leak onto, the host outside the artifact's
own cache. Single-member extraction cannot express that. This change adds
**whole-tree** extraction while keeping the single-member path byte-for-byte
unchanged for its existing callers.

## The contract

Four Pod annotations drive artifact delivery (unchanged names):

| annotation | meaning |
| --- | --- |
| `outpost.dhnt.io/native-artifact-url` | https (or loopback http) archive URL — no userinfo/query/fragment |
| `outpost.dhnt.io/native-artifact-sha256` | mandatory pinned digest of the archive — the immutable job input |
| `outpost.dhnt.io/native-artifact-path` | single-member: the one file; **tree mode: the entrypoint's slash path within the tree** |
| `outpost.dhnt.io/native-artifact-credential-profile` | optional runtime-only Secret name for a private artifact (AWS SigV4) |

The DKS workload vocabulary aliases the first three names as
`outpost.dhnt.io/executable-{url,sha256,path}`. The provider treats those as
the same declaration. If a Pod supplies both forms, their values must agree;
conflicting declarations are rejected instead of falling back to host PATH.

New in this sprint:

| annotation | meaning |
| --- | --- |
| `outpost.dhnt.io/native-artifact-tree` | `"true"` ⇒ materialize the WHOLE archive tree; `path` is then the entrypoint |

Whole-tree mode reuses `path` as the **entrypoint** rather than inventing a
fifth annotation, and reuses the existing `credential-profile` seam for private
artifacts rather than a second auth path — as the task required.

## Which single trust path — and why not binmgr

`docs/external-binary-builtins.md` states the invariant: **one
trust/verify/version path for every tool**, embodied by `coreutils/pkg/binmgr`
(download → sha256-verify → cache → run, with `Asset.Tree` + `Entrypoint` for
whole-tree extraction). The obvious question this change had to answer: route
native artifacts through binmgr, or justify a compatible path.

**Decision: extend `vknode`'s existing native-artifact path — which already IS
outpost's single content-addressed artifact trust path — from single-member to
whole-tree. No second download/verify/cache mechanism is introduced.** binmgr
remains the single path for managed external **binaries** (loom/Gitea, Zot,
SeaweedFS, Kopia, …); native artifacts remain the single path for
content-addressed **job artifacts**. The two are disjoint by *input model*, not
redundant, and this is deliberate. Four reasons binmgr is the wrong front-end
for this venue:

1. **Content-addressed vs. version-addressed.** binmgr keys its cache by
   `(name, version, platform)` and resolves an asset for the *current* platform
   from a committed spec (`GitHubSpec`/`URLSpec`) or pin. A native artifact has
   no name/version/platform matrix — it is a **signed URL + digest** carried on a
   Pod, and the digest **is** its identity. The requirement here is explicit:
   *"the extracted tree is keyed by the archive's sha256."* That is
   content-addressing; binmgr does version-addressing. Cache dir is
   `<dataDir>/artifacts/tree-<sha256>/`, so an identical archive resolves to an
   identical tree and re-extraction is skipped, and two entrypoints selecting
   into the same archive share one tree.

2. **A hostile-input acquisition front-end.** The artifact URL comes from a Pod
   annotation — a lower-trust source than a reviewed, committed binmgr spec — so
   the fetch is hardened accordingly: https-only (or loopback http), no
   userinfo/query/fragment, and a **redirect-downgrade refusal** so a 3xx can
   never move the download onto an unverified transport. binmgr's `download()` is
   a bare `http.DefaultClient.Do` with none of these guards; it doesn't need
   them for committed specs, and adding them there would burden every other
   binmgr caller.

3. **A credential-signing seam binmgr lacks.** Private artifacts are fetched with
   an **AWS SigV4**-signed request whose scope is bound to a runtime-only Secret
   (`native-artifact-credential-profile`), with the credential **never replayed
   onto a redirect**. binmgr has no request-signing hook. Routing through it
   would either drop private-artifact support or require a new seam in a shared
   coreutils package used by bashy — which this run may not touch (see the
   non-overlap contract) and which would widen binmgr's trust surface for a case
   only vknode has.

4. **binmgr's tree extractor is not hardened for this threat model.** Its
   `extractTreeTarGz` creates symlink members with **no target validation** — a
   member `link -> /etc/…` (or `../../…`) is created verbatim, and a later member
   written through it escapes the destination (the classic symlink-write-through
   RCE). It also has **no decompression-bomb bound** on extracted output. Those
   are exactly the attacks the security tests here must defend against, and since
   this run may not edit coreutils, importing that extractor would mean shipping
   vulnerabilities it could not fix.

   **Correction (sprint 42).** As originally written, this reason also asserted
   that the extractor in `native_artifact.go` "is written to defeat them." That
   was not true when it was written. The sprint-41 extractor validated symlink
   targets **lexically** with `filepath.Join`, which cleans `..` against
   components it never resolves — so a link chaining through an earlier,
   already-allowed in-tree link (`sub/link -> ".."`, then
   `esc -> "sub/link/../.."`) was lexically neutral but kernel-wise escaping, and
   a following member wrote through it **outside the tree while reporting
   success**. It differed from binmgr's extractor in the *shape* of the hole, not
   in whether it had one. The whole reason therefore rested on a comparison that
   did not hold.

   What makes the reason valid now is the fix, not the original claim: extraction
   is mediated by an `*os.Root` (below), which resolves every path component in
   the kernel and refuses anything leaving the tree. That is a different *kind* of
   defense from target validation — it removes the class rather than enumerating
   its instances. The honest form of this reason is: binmgr's extractor is
   unhardened **and** hardening it correctly means the same `os.Root` treatment,
   which a run scoped out of coreutils cannot apply there. **That fix is portable
   and binmgr still needs it** — this document does not claim otherwise, and the
   two paths stay conceptually aligned via binmgr's `Tree`/`Entrypoint` concept.

**Why this is still one trust path, not a second one.** The single-member and
whole-tree forms share the *same* front-end: one `prepareNativeArtifactURL`
(digest decode + transport + scope) and one `downloadVerifiedArtifact` (stream +
sha256 + size cap). They diverge only at the extraction step and the cache key.
No parallel verify/cache code exists — the prohibition the task set.

## Security properties of the whole-tree extractor

Archive extraction is a classic RCE surface. Every member is treated as
adversarial. The extractor lives in
`internal/agent/vknode/native_artifact.go` and is exercised by
`native_artifact_tree_test.go`, `native_artifact_tree_gate_test.go`, and
`native_artifact_tree_root_test.go` (deterministic, network-free — archives are
built in-test).

- **Evidence invariant.** "Verified" means verified: the pinned sha256 is decoded
  and enforced against the streamed bytes before anything is extracted; a
  mismatch fails loudly (`checksum mismatch`) and is never repaired by fetching
  something else. An absent/short digest is refused up front.

- **Confinement is kernel-mediated, not lexical.** Extraction opens an
  `*os.Root` on the tree directory and performs **every** member operation
  through it — `root.MkdirAll`, `root.OpenFile`, `root.Symlink`, `root.Lstat`.
  `os.Root` resolves each path component in the kernel and refuses any operation
  that would leave the root, **including one that traverses a symlink**. No
  helper builds an absolute path; they take the root and a tree-relative name.

  This replaced a lexical `filepath.Join` check that a **chained symlink**
  defeated: `filepath.Join` cleans `..` against components it never resolves, so
  `sub/link -> ".."` (in-tree, allowed) followed by `esc -> "sub/link/../.."`
  cleaned to a neutral path while the kernel climbed two levels above the tree —
  and a member written through `esc` landed outside it while `materialize`
  returned `nil`. Repeating the hop reached an arbitrary absolute path. Lexical
  path checks cannot see this; enumerating more of them is not a fix. See
  "Correction (sprint 42)" above.

- **Path traversal / zip-slip.** `safeNativeArtifactTreeName` refuses any member
  whose path is absolute (POSIX or Windows drive-qualified) or contains a `..`
  element — for tar **and** zip. `os.Root` would refuse these regardless; the
  check exists to turn a generic resolution error into a member-named `refused`
  message.

- **Symlinks.** In-tree symlinks are supported, including ones whose target is
  created by a **later** member. An escaping symlink is *inert* rather than a
  write primitive: `Root.Symlink` deliberately does not validate the link target
  (it may not exist yet), but every later member that traverses the link is
  resolved through the root and refused. Two fail-fast checks reject an absolute
  target and a lexically-obvious `..` escape at creation, purely so the operator
  gets a precise message — **they are not the containment boundary**, and the
  code says so. Regular files are additionally written `O_CREATE|O_EXCL`, so a
  member cannot overwrite a path an earlier member occupied, nor swap an
  extracted file or directory for a link.

  **What the tests actually cover** (this list is the claim; it is kept in step
  with the test files): symlink-then-write-through with a *direct* escape (tar
  and zip), asserting the outside file is untouched; absolute-target links;
  lexically escaping relative links; harmless in-tree dangling links (allowed);
  **chained** symlink write-through for tar and zip, chained-symlink arbitrary
  *absolute*-path write, and chained-symlink directory creation
  (`native_artifact_tree_gate_test.go` — the four cases that failed before the
  `os.Root` change); a **three-hop** chain that is lexically neutral at every
  step, asserting the refusal comes from root containment and not from an
  incidental `ELOOP`; a link resolved against a member declared later (must
  *succeed* — the over-blocking direction); a member replacing an already
  extracted file or directory with a symlink (TOCTOU between members); and an
  entrypoint that is itself a symlink.

- **Entrypoint.** Resolved with `root.Lstat`, not `os.Stat`, so an entrypoint
  that is a symlink is refused rather than followed, `chmod 0700`-ed, and
  executed. The mode change is applied to the open **descriptor**, never by path,
  since `Root.Chmod` on unix is documented as racy against a file→symlink swap.

- **Hardlinks.** Refused outright — a hardlink to an existing host file would be a
  read/write handle on content outside the archive, and native artifacts have no
  need for them.

- **Atomicity.** Extraction goes into a private `.materialize-*` staging tree and
  is **atomically renamed** into `tree-<sha256>` only after the entrypoint is
  present and executable. An interrupted or hostile extraction leaves **no**
  usable or partial cache entry (asserted), and concurrent extraction of the same
  digest is safe — the loser's rename fails and it recovers the winner's
  published tree.

- **Decompression bomb.** Output is bounded per-file (declared size **and**
  streamed bytes) and per-tree (cumulative bytes + member count), independent of
  the archive's compressed size. A member declaring a huge size is refused before
  it streams.

- **File modes.** Sanitized to owner-only permissions — setuid/setgid/sticky and
  all group/other bits are dropped; a member is executable only if the archive
  marked it so. The cache tree is never group/other-accessible.

## Backward compatibility

Single-member callers are unchanged: without `native-artifact-tree: "true"`,
`resolveCommand` takes the existing path, cache layout
(`<sha>-<memberkey>/executable[.exe]`), and behavior. The existing tests
(`native_artifact_test.go`) — exact-member matching, member cache separation,
private-artifact scope/redirect/query guards — continue to pass.

## Gate

```
go build ./... && go test -short -p 1 ./internal/agent/vknode/...
```
