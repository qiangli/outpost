# Peer-Hosted DKS Image Recipe Distribution

Peer image recipe distribution design and production implementation for DKS ("recipes, not blobs"; see `docs/dks-image-recipe-distribution-design.md`).

## Architecture Overview

1. **Distribute RECIPES, Not Blobs**:
   Recipes (an indexed Dockerfile + build context + canonical sha256 checksum) travel between nodes. Heavy container image blobs never move over the mesh. Base images are pulled directly from a public registry/CDN; app images are built natively and locally on every node from the recipe.
2. **Mesh Transport Boundary**:
   Recipes are served over the existing mesh forwarder (a loopback-only TCP listener exposed under the allowlisted mesh service name `recipes`). No second overlay network is introduced, and the mesh forwarder allowlist boundary remains unchanged.
3. **Four Surfaces & Single Admincore Entry**:
   All four surfaces (daemon configuration in `agent.json`, CLI subcommands under `outpost peer-image`, MCP tools `outpost_*`, and Admin UI/core) converge on ONE business-logic definition in `internal/agent/admincore/peerimage.go`.

## Configuration & Side-Effect Classes

Persisted configuration in `agent.json`:

```json
{
  "peer_image": {
    "enabled": true,
    "service": "recipes"
  }
}
```

| Field / Verb | File key / Surface | CLI | MCP Tool | Side-Effect Class | Description |
|---|---|---|---|---|---|
| Opt-in master switch | `peer_image.enabled` | `agent.json` | `outpost_set_builtins` | **Boot-only** | Enables loopback listener & mesh exposure. Default: off. Changing restarts daemon. |
| Mesh service name | `peer_image.service` | `agent.json` | `outpost_set_builtins` | **Boot-only** | Service name for recipe index. Default: `recipes`. Changing restarts daemon. |
| `publish` verb | — | `outpost peer-image publish` | `outpost_publish_image_recipe` | **Live** | Stores recipe in local store and serves to peers over mesh. |
| `mesh-resolve` verb | — | `outpost peer-image mesh-resolve` | `outpost_mesh_resolve_image_recipes` | **Live** | Finds distinct peers exposing the recipe service over mesh. |
| `ensure` verb | — | `outpost peer-image ensure` | `outpost_ensure_image` | **Live** | Materializes recipe natively into local containerd and verifies digest. |
| `report` verb | — | `outpost peer-image report` | `outpost_report_image` | **Live** | Emits identity-bound evidence (containerd digest + provenance). |

## Invariants & Security Principles

1. **Distinct Target Enforcement**:
   Operations claiming to reach $N$ nodes must prove $N$ DISTINCT nodes up front. Nodes are identified by their cluster node name (which includes per-backend discriminators), NOT by the host label `outpost.dhnt.io/host` which is shared by multiple virtual backends on the same host.
2. **Identity-Bound Evidence**:
   Every inspection challenge contains a `(node, ref, recipe_digest, nonce)` tuple. Reports must echo all four fields and match the challenge nonce. Unbound evidence or evidence with missing/mismatched nonces is refused and cannot be attributed to a node.
3. **Rejection of Duplicate & Foreign Evidence**:
   - Reports from unchallenged nodes are refused as foreign evidence.
   - Replayed nonces or duplicate reports from the same node are refused.
   - Ref and RecipeDigest mismatches are refused before evaluating content.
4. **Actual Containerd Content Digest Correlation**:
   The resident image must be confirmed by querying containerd's content digest (`sha256:<64 hex>`), NOT by podman references which can be retagged or stale. The resident content digest must match the recorded build provenance. A mismatch (`ErrDigestMismatch`) FAILS LOUDLY and is never silently repaired by pulling another image. `StateUnknown` (unreadable) and `StateAbsent` (not present) are distinct negative states.
5. **Evidence Invariant**:
   An unreachable node, empty registry listing, failed digest read, or missing report MUST fail. No success state may be reached by the ABSENCE of evidence.
6. **Safe HTTP Redirect Loopback Validation**:
   HTTP redirects are disabled in the transport client and revalidated manually on EVERY hop. Redirect chains landing on loopback/link-local addresses (other than the explicitly permitted local mesh forward port) are refused (`ErrLoopbackTarget`) to prevent SSRF against daemon admin/MCP surfaces.

## Procedure for Live Two-Host Hardware Acceptance Proof

This procedure defines the verification steps for a live two-host deployment on physical or virtual hardware.

### Hardware Status
> [!IMPORTANT]
> **HARDWARE STATUS: UNPROVEN**
> No live multi-host hardware cluster was executed during this gate run. All gate tests (`go test` and `bash script/dks-peer-image-reach_test.sh`) run offline and deterministically without external hardware or cloudbox dependencies.

### Verification Procedure (Committed Runbook)

1. **Provision Node A (`node-alpha`) and Node B (`node-beta`)**:
   - Enable peer image distribution on both nodes:
     ```bash
     outpost config set --json '{"peer_image":{"enabled":true,"service":"recipes"}}'
     outpost restart
     ```
2. **Node A Publishes Recipe**:
   ```bash
   outpost peer-image publish --name web-app --file recipe.yaml
   ```
3. **Node B Resolves Mesh Service & Fetches Recipe**:
   ```bash
   outpost peer-image mesh-resolve --service recipes --minimum 2
   outpost peer-image ensure --name web-app
   ```
4. **Inspector Challenges & Evidence Collection**:
   - Generate challenge nonces $N_A$ and $N_B$ for `(ref: "localhost/web-app:v1", recipe: "sha256:...")`.
   - Query Node A: `outpost peer-image report --node node-alpha --ref "localhost/web-app:v1" --recipe-digest "sha256:..." --nonce "$N_A"`
   - Query Node B: `outpost peer-image report --node node-beta --ref "localhost/web-app:v1" --recipe-digest "sha256:..." --nonce "$N_B"`
5. **Explicit Pass Criterion**:
   - Both `node-alpha` and `node-beta` return `state: "resident"`.
   - Both nodes return valid content digests matching `sha256:<64 hex>`.
   - Content digest on each node matches its own recorded provenance digest for `RecipeDigest`.
   - Inspector summary reports `OK: true`, `Asked: 2`, `Proven: 2`, `Rejected: []`.
