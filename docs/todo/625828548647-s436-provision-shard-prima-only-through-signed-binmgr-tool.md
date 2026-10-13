---
id: 625828548647
kind: task
title: Provision shard prima only through signed binmgr tool
seq: 1894
status: doing
priority: p2
created: 2026-10-13T14:30:00.000000Z
weave: 20
assignee: muse-conductor
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
sprint_title: 'Sphere regression recovery: p2p sharding + local inference back on, bashy-built engines'
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

State: the pinned yoke (`8e4c9f6bb5bc`) `pkg/binmgr` carries no prima tool
declaration, no workflow identity, and no SHA pins (verified by grep over the
module cache). `primaSignedTool()` therefore refuses — naming the missing
admission piece — and `ensureBinaries` stays fail-closed before any download.
No legacy on-disk cache, `$PATH` lookup, upstream shard-binaries fetch, or
ad-hoc codesign is consulted on any path. When yoke declares `EnsurePrima`
with this same contract (S436 90ea12d9), `ensureBinaries` switches to that
call; the role-wiring helper is already in place.

Delivery: `primaSignedTool` + `installVerifiedPrima` role wiring in
`provision.go`; focused tests in `provision_prima_test.go` (contract
constants, declaration refusal, legacy-cache + PATH-decoy refusal with the
bins dir untouched, both-role install wiring and role-required refusals,
gate-before-model ordering). Existing `provision_policy_test.go` refusal
unchanged and green.

Verification: `gofmt` clean, `go vet -tags fb_archives ./cmd/outpost`
clean, focused tests PASS, full `go test -tags fb_archives ./cmd/outpost`
PASS (22.5s). No native shard build on Dragon; no push (conductor pulls).
