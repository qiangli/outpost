---
id: a735418611f2
kind: bug
title: Outpost restart E2E cleanup must reap replacement daemons
seq: 2764
status: assigned
priority: p1
created: 2026-11-05T18:17:25.739392Z
assignee: muse-spark1.3
sprint: 578
sprint_id: 70ba48be-af89-5ab1-8a8d-42a15fc1ddbe
sprint_title: 'S562 follow-up: remaining fence corpus work'
---

A storage audit found old orphan test daemons retaining deleted test executables. Current e2e_test.go spawnDaemon cleanup signals and waits only its original process, while first-run SSH tests restart the daemon. Investigate whether replacement processes escape cleanup; the historical exact source identity is not proven. Add bounded lifecycle cleanup for owned replacement daemons, preserving unrelated services. Verify remotely that repeated restart E2E cases leave no owned child/replacement processes or deleted executable handles. Preserve raw evidence and exact candidate identity. Separate from the telemetry P0 deployment.

Carried from S562 (operator-approved early close 2026-11-06); deferral is not PASS. Run/branch/evidence references: see existing story details and S562 board; no verified run or branch reference recorded.
