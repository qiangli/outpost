---
id: 1003a2cac080
kind: bug
title: 'telemetry: install the slog bridge so outpost log lines reach the OTel log store as trace-correlated OTLP records'
seq: 2767
status: todo
priority: p2
labels:
    - otel
    - telemetry
created: 2026-11-10T19:58:52.776352Z
sprint: 611
sprint_id: 7de44c27-d277-58b6-b42d-e5fabc9d668f
sprint_title: 'OTel logs signal: wire outpost''s slog bridge and drain the bashy span spool into the OTel log store'
---

internal/telemetry/slog_bridge.go has provided Provider.SlogHandler() since the OTel bootstrap commit (2026-06-08) and nothing calls it, so the LoggerProvider and its otlploggrpc batch exporter built in provider.go flush nothing. the OTel log store behind otel-stack has held zero rows since bootstrap: every /insert path counter reads 0, and only July partitions from a manual import ever existed. Fix: in cmd/outpost main, after telemetry.Init succeeds and the provider has a LoggerProvider, wrap the existing stderr slog handler with p.SlogHandler() and slog.SetDefault; keep the stderr handler alone in no-op mode (SlogHandler returns nil). Verify with the stack running: a LogsQL query on /logs returns outpost lines, and lines emitted inside a gin span carry trace_id and span_id. Root cause recorded 2026-11-10 (umbrella docs/bash-command-invocation-counts.md follow-up): product gap, not a regression; no story tracked it.
