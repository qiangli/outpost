---
id: bae318dd47fb
kind: task
title: 'outpost: headlamp setup refuses (supply-chain policy, blocked on upstream GO-2026-5064/5338/5622/5932); never start the ghcr upstream image'
seq: 1890
status: done
priority: p1
labels:
    - supply-chain
created: 2026-10-13T00:23:01.792749Z
assignee: agy-gemini3.8-flash
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
closed: 2026-10-13T00:39:03.866707Z
closed_by: s434-manager
---

Conductor decision (S434, headlamp option a): headlamp stays DISABLED in outpost until a bashy-signed headlamp build exists. Its CI refuses to sign while GO-2026-5064, GO-2026-5338, GO-2026-5622 and GO-2026-5932 are reachable with no fix (blocked on upstream). The third-party image ghcr.io/headlamp-k8s/headlamp must NEVER be started.

Change (outpost only, this public repo - no proprietary references):
1. internal/agent/headlamp: Deploy (and any path that would create the headlamp Deployment: internal/agent/mcpapi/tools_bundle.go, internal/agent/bundleapply/lifecycle.go - check every caller) fails closed BEFORE touching the cluster, with an error that says: headlamp is disabled by the service supply-chain policy (no bashy-signed build yet; blocked on upstream fixes for GO-2026-5064/5338/5622/5932). Remove the upstream DefaultImage default; an Options.Image override must NOT bypass the refusal (no escape hatch for now).
2. Remove stays allowed (cleanup of an existing install must still work).
3. Unit tests (fake clientset, no network): Deploy refuses with that reason and creates NO objects; Remove still works; no code path references ghcr.io/headlamp-k8s.
Gate: cd outpost && go vet ./internal/agent/headlamp/... ./internal/agent/mcpapi/... ./internal/agent/bundleapply/... && go test -short -count=1 on the same packages. Commit with Sprint/Story trailers, then bashy sprint submit with SHA + gate output.
