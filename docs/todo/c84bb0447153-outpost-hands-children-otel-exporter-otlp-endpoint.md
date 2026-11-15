---
id: c84bb0447153
kind: bug
title: outpost hands children OTEL_EXPORTER_OTLP_ENDPOINT as host:port; bashy's HTTP OTLP exporters want a URL (parse error + export to :4318 on every child run)
seq: 2775
status: todo
priority: p1
labels:
    - telemetry
created: 2026-11-15T05:43:19.440637Z
---

Seen 2026-11-15 on the Windows QA host (outpost v1.0.0-rc.33-dev daemon): every bashy.exe spawned through outpost (sshd, apps) inherits OTEL_EXPORTER_OTLP_ENDPOINT=127.0.0.1:31416, OTEL_SERVICE_NAME=outpost, OTEL_RESOURCE_ATTRIBUTES=host.name=... (outpost e7ebb11 'telemetry: pin managed children to local OTLP', gRPC form per internal/telemetry/provider.go). bashy's OTel SDK uses HTTP exporters whose envconfig parses the value as a URL: stderr 'internal_logging.go:50 msg=parse url error=parse "127.0.0.1:31416": first path segment in URL cannot contain colon', then 'traces export: Post https://localhost:4318/v1/traces ... refused' on every command. It broke two rc.34 tier probes that parse command output (venue-neutralised in the kit by unsetting the vars). Fix: export a full URL (scheme://host:port) that both gRPC and HTTP exporters accept, or set the protocol var too; and bashy should not print exporter failures to the user's stderr.

CORRECTION 2026-11-15 06:40Z: outpost main's ChildEnv (internal/telemetry/provider.go) already exports a URL (http://127.0.0.1:31415) plus OTEL_EXPORTER_OTLP_*_PROTOCOL=http/protobuf, and so did the installed daemon's code (2a07363). The Windows QA host's daemon PROCESS was launched (2026-11-14 07:56) from a shell carrying OTEL_EXPORTER_OTLP_ENDPOINT=127.0.0.1:31416 (yoke otel stack gRPC bridge, host:port form), OTEL_SERVICE_NAME=outpost, OTEL_RESOURCE_ATTRIBUTES=host.name=<host> — an operator/session experiment, not in the registry, inherited by every sshd-spawned shell. The second Windows host has no such env. Remaining product part (bashy): the OTel SDK's envconfig/export errors go to the user's stderr on every command when the inherited endpoint is unusable; bashy should route SDK diagnostics to its log, not the terminal. Venue: restart that daemon from a clean environment (or clear the vars) — tier kit neutralises via a PowerShell launcher that removes the vars before starting the candidate.
