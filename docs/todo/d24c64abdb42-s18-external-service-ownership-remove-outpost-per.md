---
id: d24c64abdb42
kind: feature
title: 'S18 external service ownership: remove outpost per-service launch paths and bind Zot/SeaweedFS/Kopia/act_runner/Headlamp to generic units'
seq: 1888
status: done
priority: p1
labels:
    - outpost
    - service
created: 2026-10-11T08:33:02.735808Z
weave: 4
assignee: claude-opus5
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T23:19:46.260435Z
resolution: fixed
closed_by: claude-opus5.5
---

Child of S18 d7f31dbff83b. Source review of outpost 6d4ffaf found cmd/outpost/main.go still starts Zot/SeaweedFS/Kopia/act_runner and other external services behind FileConfig booleans despite S18 generic svcunit reconciliation. Replace these old per-service boot/mesh paths with Bashy service unit registry definitions and generic monitor/exposure plumbing. Preserve version pins, provisioning, data dirs, ports, labels/token/sandbox/DOCKER_HOST/vault, retry/upgrade behavior and Headlamp port-forward feature. Remove old config formats/readers directly; no compatibility/migration. Coordinate with yoke S17 external seed child 90064599529f: do not duplicate unit YAML there; report any schema/provisioning contract gap. Scope outpost cmd/outpost/main.go, conf readers/tests and matching docs; do not edit Bashy/Yoke source. Required worker verify: GOWORK=off go test -short ./cmd/outpost/... ./internal/agent/conf/... ./internal/agent/svcunit/...; git diff --check; state remaining inventory. Manager independently runs Linux gate with paired Bashy and live enable/withdraw test. Original S18 remains open until all internal/external surfaces and two-host gates pass.
