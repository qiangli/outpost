---
id: 45e5c45732e2
kind: bug
title: Use Yoke EnsurePrima as Outpost shard verifier
seq: 1895
status: assigned
priority: p1
created: 2026-10-13T14:14:42.399423Z
weave: 21
assignee: codex-gpt6-astra
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
sprint_title: 'Sphere regression recovery: p2p sharding + local inference back on, bashy-built engines'
---

Correction to Outpost interim a713c58c / Story 625828548647. yoke/pkg/binmgr/prima.go now provides PrimaTool and EnsurePrima with immutable repo, release tag, exact identity/issuer and committed-pin fail closed. Remove duplicate Outpost primaReleaseRepo, primaWorkflowIdentity, primaSHA256Pins and primaSignedTool; ensureBinaries must call binmgr.EnsurePrima(ctx) and install only verified path for leader/worker roles. Keep fail closed until signed assets and pins land. Reconcile duplicate docs/todo/625828548647 story files: worker-created tracked s436-... and generated untracked original; retain one canonical file in worker commit. Add focused tests proving call path and no fallback. Worker alone commits with trailers and story files, no push or Dragon native build/container.


Delivery: removed Outpost's duplicate Prima declaration and delegated directly
to `binmgr.EnsurePrima(ctx)`. Pinned Yoke to 17336a2facb6 (published module),
which includes committed-pin refusal and offline cached-bundle verification.
Only the verified returned path supplies both shard roles. No signed assets,
SHA pins, or live execution are claimed; provisioning remains fail closed.

Reconciled 625828548647 to the generated canonical filename, carrying its
blocked state and removing the interim worker-created duplicate.

Verification (2026-10-13):
- Six focused provisioning tests PASS, including the real shared verifier's
  wrapped refusal, zero network/cache/role writes, no PATH/codesign/curl fallback,
  unchanged legacy bytes, both-role install and gate-before-model behavior.
- `go vet -tags fb_archives ./cmd/outpost` PASS; `git diff --check` PASS.
- `go test -tags fb_archives ./...`: CLI package PASS (31.512s); all tested
  packages PASS except the existing svcunit test
  `TestSeededBuiltinInternalAndCustomShareOneMachinery` at store_test.go:233
  ("a seeded unit must default to disabled"). Reproduced that exact failure
  using HEAD's original go.mod/go.sum through an isolated -modfile, confirming
  it predates this dependency update. Full gate is not green.

No push, native engine build, or container run. Manager acceptance remains
pending; the signed-asset/pin prerequisite remains open in the parent story.
