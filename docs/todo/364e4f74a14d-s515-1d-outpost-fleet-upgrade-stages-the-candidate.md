---
id: 364e4f74a14d
kind: bug
title: S515.1d outpost fleet upgrade stages the candidate but never swaps it in
seq: 2761
status: todo
priority: p1
created: 2026-10-30T08:19:54.24481Z
sprint: 515
sprint_id: e82160ea-95db-5f2a-b69e-b2d784b2996b
sprint_title: 'satellite loom: private forge + CI + groups (KISS/MVP, reuse loom/act_runner/scopes)'
---

S515.1 companion (operator decision 2026-10-30: P2 investigated inside S515.1, bounded 60 min). Symptom: a host fleet upgrade request (release v1.0.0-rc.23-dev, sha256-verified asset) returns 202 accepted with detail "staging candidate", but 13+ minutes later the running outpost is still the old version and its daemon start time is unchanged: the candidate is staged and never swapped in. Host: a fresh Linux VM whose outpost was installed from release v1.0.0-rc.10-dev and runs as the root service. Code: internal/agent/upgrade/worker.go (staging) and the swap/restart path. Task: find why stage never swaps (e.g. swap gated on a condition the old version never meets, missing restart trigger, health gate failing silently, version/semver compare of -dev tags), fix with a red/green unit test if the fix is small; otherwise record the root cause precisely and stop. Also add an agent-readable upgrade status (last stage/swap result + error) if trivial. No live host changes, no release cut. Hostnames never in this repo.
