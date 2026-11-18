---
id: a24b2a877bd1
kind: task
title: Cut consumer package references to separated source repositories
seq: 2781
status: assigned
created: 2026-11-18T04:14:20.377111Z
assignee: s575-consumer-refs
sprint: 575
sprint_id: 65ce85f5-87c0-5863-bf55-62de86f5df36
sprint_title: 'Layer separation: Classic <- Bash# <- Yoke import-graph delineation'
---

Sprint 575 reference-only repository cutover. Preserve implementation and tests; update package/module references and fixed published lower dependency pins. Official-image go build is performed by the manager; no local Go execution.
