---
id: a24b2a877bd1
kind: task
title: Cut consumer package references to separated source repositories
seq: 2781
status: done
created: 2026-11-18T04:14:20.377111Z
assignee: s575-codex-manager
sprint: 575
sprint_id: 65ce85f5-87c0-5863-bf55-62de86f5df36
sprint_title: 'Layer separation: Classic <- Bash# <- Yoke import-graph delineation'
closed: 2026-11-20T12:26:28.874536Z
closed_by: s575-codex-manager
---

Sprint 575 reference-only repository cutover. Preserve implementation and tests; update package/module references and fixed published lower dependency pins. Official-image go build is performed by the manager; no local Go execution.

## Manager closeout — 2026-11-20
Published reference cutover 0e1e2cb1 is covered by retained official published-consumer build and startup/config/supervisor qualification at 634af55 for the unchanged references. No full current-head test or unrelated release-fix acceptance is claimed.
Existing receipts and source ancestry were reviewed; no new test/build ran during reconciliation. Evidence belongs to its exact historical source/artifact identities, not arbitrary later heads. S575 full-run evidence remains under posix-baseline/s575-2026-11-19 with its raw 4 FAIL, 54 UNRESOLVED and 9 INSPECT unchanged. RC38 is on operator hold.
