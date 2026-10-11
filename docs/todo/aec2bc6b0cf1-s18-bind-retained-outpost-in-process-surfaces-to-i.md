---
id: aec2bc6b0cf1
kind: feature
title: S18 bind retained outpost in-process surfaces to internal service units
seq: 1889
status: todo
priority: p1
labels:
    - outpost
    - service
created: 2026-10-11T09:12:43.123921Z
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Scoped child of S18 d7f31dbff83b after external ownership child d24c64abdb42. In-process outpost handlers retain their owning subsystems but must use the SAME service registry declaration/config/enablement/status/exposure machinery as built-in and external units. Bind internal capabilities outpost.files (File Browser allow_write/scope and login), outpost.podman (admin role/socket detection), outpost.sandbox (memory/cpus/pids/max containers/images/scratch/prewarm), outpost.ollama and outpost.ollama-pool, outpost.warm, outpost.shard, outpost.lan-inference, outpost.otel and outpost.ycode. Preserve all existing behavior, supported settings, proxy/intercept paths, port conflicts, secrets/security/identity, pool/warm/shard health, and mesh/app exposure. Remove old parallel enablement/config keys and direct lifecycle paths without compatibility/migration; map admin/MCP toggles onto bashy service enable|disable|set and status. Use the shared internal provider contract from S15; no service-name conditionals in the generic reconciler. Source of exact inventory: umbrella docs/bashy-service-design.md Retained service and feature coverage table. Start after external S18 candidate merges to avoid cmd/outpost/main.go overlap. Worker verify GOWORK=off go test -short ./cmd/outpost/... ./internal/agent/conf/... ./internal/agent/svcunit/... ./internal/agent/admincore/... plus relevant sandbox/ollama/warm/shard tests; no skipped legacy regression without a replacement assertion. Manager independently runs Linux full outpost gate and paired-host live toggles. Original S18 remains open until complete coverage and exposure verification.
