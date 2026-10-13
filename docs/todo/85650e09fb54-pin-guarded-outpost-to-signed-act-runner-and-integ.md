---
id: 85650e09fb54
kind: chore
title: Pin guarded outpost to signed act_runner and integrated sphere-default yoke
seq: 1892
status: done
priority: p1
created: 2026-10-13T08:21:36.323131Z
weave: 17
assignee: s434-pins
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
closed: 2026-10-13T08:29:28.748708Z
resolution: fixed
closed_by: s434-pins-q
---

Integration for S434 closeout4b88c34b/f1be2620. Work ONLY isolated outpost. Exact base origin/main b2749c8faa5f1711f7575dacc3f2ffb325c161cf (S436 URL+migration+prima refusal). Exact producer yoke origin/main b0f5fd30 contains signed act_runner260bfd20 plus S436 defaults and checksum fixes; verify full SHA with ls-remote. Update go.mod and go.sum to that exact yoke and related yoke submodules as needed, keep Go1.27.2, no broad upgrades. Preserve guarded prima refusal, do not change production behavior beyond dependency bump. Build with GOWORK=off; focused go test -short -tags fb_archives ./cmd/outpost -run Actrunner plus relevant config migration tests; inspect CLAUDE for actual tag if different. No full build/suite or containers on Dragon. Commit scoped dependency files plus own story with Sprint434 Story-ID trailers; no push or main mutation. Report exact yoke full SHA/pseudo-version, module graph changes, tests. Manager coordinates S436; yoke/outpost main holds acquired. No other repo changes, no secrets. ACK exact target to s434-codex-manager.
