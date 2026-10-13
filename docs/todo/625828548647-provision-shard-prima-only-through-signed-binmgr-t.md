---
id: "625828548647"
kind: task
title: Provision shard prima only through signed binmgr tool
seq: 1894
status: blocked
priority: p2
created: 2026-10-13T14:30:00Z
weave: 20
assignee: muse-conductor
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
sprint_title: 'Sphere regression recovery: p2p sharding + local inference back on, bashy-built engines'
resolution: fixed
closed_by: muse-conductor-t
---

Substory of Sprint 436 17ba375e and prerequisite for 8998b0dc live proof.

`cmd/outpost/provision.go` failed closed in `ensureBinaries` after removing
the old shard-binaries GitHub-release download and ad-hoc codesign
(b2749c8). This story replaces that stub with the signed binmgr path: once
the yoke-side signed tool declaration exists, provisioning resolves it and
calls `binmgr.Ensure`, installing the verified binary for the required shard
server (leader rank 0) and worker roles.

Contract: source `bashy-prima-0.1.0` peeled to
`f4ea9845dc91a18793921492e50ef23ec799e93e`, generated release
`bashy-bashy-prima-0.1.0-1`, assets `prima-{linux,darwin}-{amd64,arm64}` raw
executables with SHA256SUMS and per-asset `.sigstore.json`, exact GitHub
workflow identity and OIDC issuer declared in binmgr.

Correction under story 45e5c45732e2: Outpost now delegates exclusively to
`binmgr.EnsurePrima(ctx)` and installs only its returned verified path for
both leader and worker roles. The duplicate local release declaration is
removed. Yoke is pinned to 17336a2facb6, including committed-pin enforcement
and cached Sigstore bundle verification. Missing pins still refuse before
network/cache access; no signed assets or live shard proof are claimed.

The generated story filename is canonical; the interim worker-created
`s436-...` duplicate is removed. This story remains blocked on signed assets
and committed SHA pins. Verification of the consumer correction is recorded
in story 45e5c45732e2.
