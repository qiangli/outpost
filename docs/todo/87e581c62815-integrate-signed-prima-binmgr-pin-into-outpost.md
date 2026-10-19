---
id: 87e581c62815
kind: chore
title: Integrate signed Prima binmgr pin into Outpost
seq: 1896
status: done
priority: p1
created: 2026-10-13T15:21:59.008887Z
weave: 22
assignee: codex-gpt6-astra
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
sprint_title: 'Sphere regression recovery: p2p sharding + local inference back on, bashy-built engines'
closed: 2026-10-13T16:28:24.49566Z
closed_by: s436-codex-manager
---

After Yoke commits real four-platform SHA pins and the final signed Prima 0.1.1 release version with independent Sigstore verification, bump Outpost Yoke module to the pushed commit and verify provision.go direct binmgr.EnsurePrima path remains fail-closed. Focused Go test/vet, no dev-host heavy build/container. Commit worker story file and code with trailers; no push. Manager integrates/pushes and bumps umbrella pin.

Delivery (2026-10-13):

- Updated Yoke and its llmgw/oci replacement pins to
  `v0.0.0-20261013162229-73d10206aa2d`. `git ls-remote` confirmed pushed
  main `73d10206aa2d4df1caa5a1aa8b5a1b34158eb1af`; Go resolved and downloaded
  that exact commit with `GOWORK=off`.
- Prerequisite evidence: Yoke story `2010e4b56b20`, accepted by
  `s436-codex-manager` at 2026-10-13T16:23:41Z, records independent offline
  Sigstore verification of signed SHA256SUMS and four raw executable bundles
  for `bashy-bashy-prima-0.1.1-4`. The manager additionally verified all 34
  release bundles, all four SLSA attestations, and byte equality with hosted
  run 38427264223 artifacts. This worker reviewed that evidence and the
  committed four-platform pins; it did not repeat the release download.
- Reviewed `cmd/outpost/provision.go`: direct `binmgr.EnsurePrima(ctx)` call
  remains intact. Errors return before role installation, stale-process
  cleanup, or model provisioning. Only the verified path supplies both roles.
- Replaced tests' obsolete missing-pin assumption with isolated unsigned
  cache rejection, preserving the real verifier call. Added failed-download
  coverage. All tests avoid the host cache and external artifact downloads;
  legacy role bytes and PATH decoys cannot bypass verification.

Gate results, inspected for actual executed tests and assertions:

- `GOWORK=off go test -tags fb_archives ./cmd/outpost -run
  'Test(EnsureBinaries|InstallVerifiedPrima|ProvisionShardGate|LegacyPrima|ParseModelRef)'
  -count=1 -v`: PASS, eight tests, none skipped on darwin/arm64.
- `GOWORK=off go test github.com/qiangli/yoke/pkg/binmgr -run 'TestPrima'
  -count=1 -v`: PASS, declaration/four real SHA pins, signed provisioning and
  cache refusals, strict signed-manifest rows (11 subtests).
- `GOWORK=off go vet -tags fb_archives ./cmd/outpost
  github.com/qiangli/yoke/pkg/binmgr`: PASS.
- `git diff --check`: PASS.

No full suite, native engine build, container, or live two-host proof was run.
Manager owns acceptance, integration/push, and the umbrella pin update.
