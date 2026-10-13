---
id: 48d04d44b462
kind: task
title: S434 final outpost yoke pin after S436 managed Ollama fail-closed guard
seq: 1893
status: done
priority: p0
created: 2026-10-13T08:49:48.811474Z
weave: 18
assignee: s434-final-pins
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
closed: 2026-10-13T08:58:04.19077Z
resolution: fixed
closed_by: s434-final-pins-r
---

Implement dependency-only follow-up to accepted85650e09. Base outpost main1a4df7e atop065b77d8. Exact PUSHED yoke target43e1f4159979de01ecec57635085a1632cfba131 includes signed act_runner, defaults, and managed Ollama fail-closed guard. Verify ls-remote/fetch exact target. Update root and nested module dependency files only as needed, retain Go1.27.2 and guarded prima refusal. GOWORK=off tidy diff/mod verify plus focused tagged Actrunner/config migration tests (see prior85650 evidence). No full build or broad package test on Dragon; native build deferred. Do not start services/containers or access remote fixtures. Commit dependency files and own story1893/48d04d44b462 with Sprint434 trailers; independently claim and submit this story as s434-final-pins. No main mutation or push. Reply receipt/progress to s434-codex-manager. Story file is in docs/todo; no broad filesystem searching.
