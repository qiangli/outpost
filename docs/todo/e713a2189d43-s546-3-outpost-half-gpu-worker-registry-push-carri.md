---
id: e713a2189d43
kind: feature
title: 'S546.3 (outpost half): GPU worker registry push carries worker{request_id, vram_bytes, gpu_model}'
seq: 2762
status: assigned
priority: p1
created: 2026-10-31T02:08:34.325629Z
weave: 109
assignee: claude-opus5.5
sprint: 546
sprint_id: ae111a75-889f-50a4-a423-de1bab0fbca6
sprint_title: On-demand cloud GPU workers for large/custom Ollama models via Cloudbox and Outpost (AWS EC2 first)
---

Second half of S546.3 (11d5bc278666), split only because work runs are per repo; not a new feature. The master side of POST /api/v1/llm/registry now accepts an optional top-level "worker" object on a push: {"request_id": string (1-64 chars, the provision request id this host was created for), "vram_bytes": int64 >= 0, "gpu_model": string (<= 64 chars)}. Present only on a provisioned cloud GPU worker; absent on every other host (an invalid block is rejected with HTTP 400). Today outpost never sends it (only aggregate_vram_bytes for clusters). Scope: internal/agent/ollama watcher push adds the worker block when this host is a provisioned GPU worker: request_id from the worker bootstrap (define ONE outpost config field/env the bootstrapper sets, document it in outpost docs), vram_bytes + gpu_model measured on the host (prefer the model server's own report or /proc/driver/nvidia; no distro packages; fail soft: omit the block and log when unmeasurable), cached models as today. Red first: watcher push test asserting the worker block from a fake bootstrap value + fake VRAM source; non-worker hosts unchanged. Gate: go test -short ./internal/agent/ollama/... + go vet ./internal/agent/... OSS repo: describe only the public wire shape; no hostnames, account ids or IPs. Bashy-only mechanisms.
