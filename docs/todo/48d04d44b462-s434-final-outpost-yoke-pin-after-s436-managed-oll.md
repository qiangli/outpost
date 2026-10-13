---
id: 48d04d44b462
kind: task
title: S434 final outpost yoke pin after S436 managed Ollama fail-closed guard
seq: 1893
status: assigned
priority: p0
created: 2026-10-13T08:49:48.811474Z
assignee: s434-final-pins
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
---

Dependency-only follow-up to accepted 85650e09. Base outpost065b77d8; target exact pushed S436 yoke managed-serve guard SHA (manager will supply). Update root and nested module dependency files only as needed, retain Go1.27.2 and guarded prima refusal. GOWORK=off tidy diff/mod verify plus focused tagged Actrunner/config migration tests; no heavy build on Dragon. Commit with numeric Story and Story-ID trailers; submit to manager, no main mutation/push.
